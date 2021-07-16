package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/gomarkdown/markdown"
	gomatrix "maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type MatrixDaemon struct {
	Stopped         bool
	ShutdownChannel chan string
	bot             *gomatrix.Client
	userID          id.UserID
	tokens          map[string]UnverifiedUser // Map of tokens to users
	languages       map[id.RoomID]string      // Map of roomIDs to language codes
	Encryption      bool
	isEncrypted     map[id.RoomID]bool
	cryptoStore     *crypto.GobStore
	olm             *crypto.OlmMachine
	app             *appContext
	start           int64
}

type UnverifiedUser struct {
	Verified bool
	User     *MatrixUser
}

type MatrixUser struct {
	RoomID    string
	Encrypted bool
	UserID    string
	Lang      string
	Contact   bool
}

var matrixFilter = gomatrix.Filter{
	Room: gomatrix.RoomFilter{
		Timeline: gomatrix.FilterPart{
			Types: []event.Type{
				event.EventMessage,
				event.EventEncrypted,
				event.StateMember,
			},
		},
	},
	EventFields: []string{
		"type",
		"event_id",
		"room_id",
		"state_key",
		"sender",
		"content",
		"timestamp",
		// "content.body",
		// "content.membership",
	},
}

func newMatrixDaemon(app *appContext) (d *MatrixDaemon, err error) {
	matrix := app.config.Section("matrix")
	homeserver := matrix.Key("homeserver").String()
	token := matrix.Key("token").String()
	d = &MatrixDaemon{
		ShutdownChannel: make(chan string),
		userID:          id.UserID(matrix.Key("user_id").String()),
		tokens:          map[string]UnverifiedUser{},
		languages:       map[id.RoomID]string{},
		isEncrypted:     map[id.RoomID]bool{},
		app:             app,
		start:           time.Now().UnixNano() / 1e6,
	}
	d.bot, err = gomatrix.NewClient(homeserver, d.userID, token)
	if err != nil {
		return
	}
	// resp, err := d.bot.CreateFilter(&matrixFilter)
	// if err != nil {
	// 	return
	// }
	// d.bot.Store.SaveFilterID(d.userID, resp.FilterID)
	for _, user := range app.storage.matrix {
		if user.Lang != "" {
			d.languages[id.RoomID(user.RoomID)] = user.Lang
		}
		d.isEncrypted[id.RoomID(user.RoomID)] = user.Encrypted
	}
	err = InitMatrixCrypto(d)
	return
}

func (d *MatrixDaemon) generateAccessToken(homeserver, username, password string) (string, error) {
	req := &gomatrix.ReqLogin{
		Type: gomatrix.AuthTypePassword,
		Identifier: gomatrix.UserIdentifier{
			Type: gomatrix.IdentifierTypeUser,
			User: username,
		},
		Password: password,
		DeviceID: id.DeviceID("jfa-go-" + commit),
	}
	bot, err := gomatrix.NewClient(homeserver, id.UserID(username), "")
	if err != nil {
		return "", err
	}
	resp, err := bot.Login(req)
	if err != nil {
		return "", err
	}
	return resp.AccessToken, nil
}

func (d *MatrixDaemon) run() {
	startTime := d.start
	d.app.info.Println("Starting Matrix bot daemon")
	syncer := d.bot.Syncer.(*gomatrix.DefaultSyncer)
	HandleSyncerCrypto(startTime, d, syncer)
	syncer.OnEventType(event.EventMessage, d.handleMessage)

	if err := d.bot.Sync(); err != nil {
		d.app.err.Printf("Matrix sync failed: %v", err)
	}
}

func (d *MatrixDaemon) Shutdown() {
	CryptoShutdown(d)
	d.bot.StopSync()
	d.Stopped = true
	close(d.ShutdownChannel)
}

func (d *MatrixDaemon) handleMessage(source gomatrix.EventSource, evt *event.Event) {
	if evt.Timestamp < d.start {
		return
	}
	if evt.Sender == d.userID {
		return
	}
	fmt.Printf("RECV %+v\n", evt.Content)
	lang := "en-us"
	if l, ok := d.languages[evt.RoomID]; ok {
		if _, ok := d.app.storage.lang.Telegram[l]; ok {
			lang = l
		}
	}
	sects := strings.Split(evt.Content.Raw["body"].(string), " ")
	switch sects[0] {
	case "!lang":
		if len(sects) == 2 {
			d.commandLang(evt, sects[1], lang)
		} else {
			d.commandLang(evt, "", lang)
		}
	}
}

func (d *MatrixDaemon) commandLang(evt *event.Event, code, lang string) {
	if code == "" {
		list := "!lang <lang>\n"
		for c := range d.app.storage.lang.Telegram {
			list += fmt.Sprintf("%s: %s\n", c, d.app.storage.lang.Telegram[c].Meta.Name)
		}
		_, err := d.bot.SendText(
			evt.RoomID,
			list,
		)
		if err != nil {
			d.app.err.Printf("Matrix: Failed to send message to \"%s\": %v", evt.Sender, err)
		}
		return
	}
	if _, ok := d.app.storage.lang.Telegram[code]; !ok {
		return
	}
	d.languages[evt.RoomID] = code
	if u, ok := d.app.storage.matrix[string(evt.RoomID)]; ok {
		u.Lang = code
		d.app.storage.matrix[string(evt.RoomID)] = u
		if err := d.app.storage.storeMatrixUsers(); err != nil {
			d.app.err.Printf("Matrix: Failed to store Matrix users: %v", err)
		}
	}
}

func (d *MatrixDaemon) CreateRoom(userID string) (roomID id.RoomID, encrypted bool, err error) {
	var room *gomatrix.RespCreateRoom
	room, err = d.bot.CreateRoom(&gomatrix.ReqCreateRoom{
		Visibility: "private",
		Invite:     []id.UserID{id.UserID(userID)},
		Topic:      d.app.config.Section("matrix").Key("topic").String(),
		IsDirect:   true,
	})
	if err != nil {
		return
	}
	encrypted = EncryptRoom(d, room, id.UserID(userID))
	roomID = room.RoomID
	return
}

func (d *MatrixDaemon) SendStart(userID string) (ok bool) {
	roomID, encrypted, err := d.CreateRoom(userID)
	if err != nil {
		d.app.err.Printf("Failed to create room for user \"%s\": %v", userID, err)
		return
	}
	lang := "en-us"
	pin := genAuthToken()
	d.tokens[pin] = UnverifiedUser{
		false,
		&MatrixUser{
			RoomID:    string(roomID),
			UserID:    userID,
			Lang:      lang,
			Encrypted: encrypted,
		},
	}
	err = d.send(
		&event.MessageEventContent{
			MsgType: event.MsgText,
			Body: d.app.storage.lang.Telegram[lang].Strings.get("matrixStartMessage") + "\n\n" + pin + "\n\n" +
				d.app.storage.lang.Telegram[lang].Strings.template("languageMessage", tmpl{"command": "!lang"}),
		},
		roomID,
	)
	if err != nil {
		d.app.err.Printf("Matrix: Failed to send welcome message to \"%s\": %v", userID, err)
		return
	}
	ok = true
	return
}

func (d *MatrixDaemon) send(content *event.MessageEventContent, roomID id.RoomID) (err error) {
	if encrypted, ok := d.isEncrypted[roomID]; ok && encrypted {
		err = SendEncrypted(d, content, roomID)
	} else {
		_, err = d.bot.SendMessageEvent(roomID, event.NewEventType("m.room.message"), content, gomatrix.ReqSendEvent{})
	}
	if err != nil {
		return
	}
	return
}

func (d *MatrixDaemon) Send(message *Message, users ...MatrixUser) (err error) {
	md := ""
	if message.Markdown != "" {
		// Convert images to links
		md = string(markdown.ToHTML([]byte(strings.ReplaceAll(message.Markdown, "![", "[")), nil, renderer))
	}
	content := &event.MessageEventContent{
		MsgType: "m.text",
		Body:    message.Text,
	}
	if md != "" {
		content.FormattedBody = md
		content.Format = "org.matrix.custom.html"
	}
	for _, user := range users {
		if user.Encrypted {
			err = SendEncrypted(d, content, id.RoomID(user.RoomID))
		} else {
			err = d.send(content, id.RoomID(user.RoomID))
		}
		if err != nil {
			return
		}
	}
	return
}

// User enters ID on sign-up, a PIN is sent to them. They enter it on sign-up.

// Message the user first, to avoid E2EE by default
