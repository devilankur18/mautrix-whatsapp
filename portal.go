// mautrix-whatsapp - A Matrix-WhatsApp puppeting bridge.
// Copyright (C) 2019 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"mime"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/Rhymen/go-whatsapp"
	waProto "github.com/Rhymen/go-whatsapp/binary/proto"

	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix-appservice"

	"maunium.net/go/mautrix-whatsapp/database"
	"maunium.net/go/mautrix-whatsapp/types"
	"maunium.net/go/mautrix-whatsapp/whatsapp-ext"
)

func (bridge *Bridge) GetPortalByMXID(mxid types.MatrixRoomID) *Portal {
	bridge.portalsLock.Lock()
	defer bridge.portalsLock.Unlock()
	portal, ok := bridge.portalsByMXID[mxid]
	if !ok {
		return bridge.loadDBPortal(bridge.DB.Portal.GetByMXID(mxid), nil)
	}
	return portal
}

func (bridge *Bridge) GetPortalByJID(key database.PortalKey) *Portal {
	bridge.portalsLock.Lock()
	defer bridge.portalsLock.Unlock()
	portal, ok := bridge.portalsByJID[key]
	if !ok {
		return bridge.loadDBPortal(bridge.DB.Portal.GetByJID(key), &key)
	}
	return portal
}

func (bridge *Bridge) GetAllPortals() []*Portal {
	return bridge.dbPortalsToPortals(bridge.DB.Portal.GetAll())
}

func (bridge *Bridge) GetAllPortalsByJID(jid types.WhatsAppID) []*Portal {
	return bridge.dbPortalsToPortals(bridge.DB.Portal.GetAllByJID(jid))
}

func (bridge *Bridge) dbPortalsToPortals(dbPortals []*database.Portal) []*Portal {
	bridge.portalsLock.Lock()
	defer bridge.portalsLock.Unlock()
	output := make([]*Portal, len(dbPortals))
	for index, dbPortal := range dbPortals {
		if dbPortal == nil {
			continue
		}
		portal, ok := bridge.portalsByJID[dbPortal.Key]
		if !ok {
			portal = bridge.loadDBPortal(dbPortal, nil)
		}
		output[index] = portal
	}
	return output
}

func (bridge *Bridge) loadDBPortal(dbPortal *database.Portal, key *database.PortalKey) *Portal {
	if dbPortal == nil {
		if key == nil {
			return nil
		}
		dbPortal = bridge.DB.Portal.New()
		dbPortal.Key = *key
		dbPortal.Insert()
	}
	portal := bridge.NewPortal(dbPortal)
	bridge.portalsByJID[portal.Key] = portal
	if len(portal.MXID) > 0 {
		bridge.portalsByMXID[portal.MXID] = portal
	}
	return portal
}

func (portal *Portal) GetUsers() []*User {
	return nil
}

func (bridge *Bridge) NewPortal(dbPortal *database.Portal) *Portal {
	portal := &Portal{
		Portal: dbPortal,
		bridge: bridge,
		log:    bridge.Log.Sub(fmt.Sprintf("Portal/%s", dbPortal.Key)),

		recentlyHandled: [recentlyHandledLength]types.WhatsAppMessageID{},

		messages: make(chan PortalMessage, 128),
	}
	go portal.handleMessageLoop()
	return portal
}

const recentlyHandledLength = 100

type PortalMessage struct {
	chat      string
	source    *User
	data      interface{}
	timestamp uint64
}

type Portal struct {
	*database.Portal

	bridge *Bridge
	log    log.Logger

	roomCreateLock sync.Mutex

	recentlyHandled      [recentlyHandledLength]types.WhatsAppMessageID
	recentlyHandledLock  sync.Mutex
	recentlyHandledIndex uint8

	backfillLock  sync.Mutex
	backfilling   bool
	lastMessageTs uint64

	privateChatBackfillInvitePuppet func()

	messages chan PortalMessage

	isPrivate *bool
}

const MaxMessageAgeToCreatePortal = 5 * 60 // 5 minutes

func (portal *Portal) handleMessageLoop() {
	for msg := range portal.messages {
		if len(portal.MXID) == 0 {
			if msg.timestamp+MaxMessageAgeToCreatePortal < uint64(time.Now().Unix()) {
				portal.log.Debugln("Not creating portal room for incoming message as the message is too old.")
				continue
			}
			err := portal.CreateMatrixRoom(msg.source)
			if err != nil {
				portal.log.Errorln("Failed to create portal room:", err)
				return
			}
		}
		portal.backfillLock.Lock()
		portal.handleMessage(msg)
		portal.backfillLock.Unlock()
	}
}

func (portal *Portal) handleMessage(msg PortalMessage) {
	if len(portal.MXID) == 0 {
		portal.log.Warnln("handleMessage called even though portal.MXID is empty")
		return
	}
	switch data := msg.data.(type) {
	case whatsapp.TextMessage:
		portal.HandleTextMessage(msg.source, data)
	case whatsapp.ImageMessage:
		portal.HandleMediaMessage(msg.source, data.Download, data.Thumbnail, data.Info, data.Type, data.Caption)
	case whatsapp.VideoMessage:
		portal.HandleMediaMessage(msg.source, data.Download, data.Thumbnail, data.Info, data.Type, data.Caption)
	case whatsapp.AudioMessage:
		portal.HandleMediaMessage(msg.source, data.Download, nil, data.Info, data.Type, "")
	case whatsapp.DocumentMessage:
		portal.HandleMediaMessage(msg.source, data.Download, data.Thumbnail, data.Info, data.Type, data.Title)
	case whatsappExt.MessageRevocation:
		portal.HandleMessageRevoke(msg.source, data)
	case FakeMessage:
		portal.HandleFakeMessage(msg.source, data)
	default:
		portal.log.Warnln("Unknown message type:", reflect.TypeOf(msg.data))
	}
}

func (portal *Portal) isRecentlyHandled(id types.WhatsAppMessageID) bool {
	start := portal.recentlyHandledIndex
	for i := start; i != start; i = (i - 1) % recentlyHandledLength {
		if portal.recentlyHandled[i] == id {
			return true
		}
	}
	return false
}

func (portal *Portal) isDuplicate(id types.WhatsAppMessageID) bool {
	msg := portal.bridge.DB.Message.GetByJID(portal.Key, id)
	if msg != nil {
		return true
	}
	return false
}

func init() {
	gob.Register(&waProto.Message{})
}

func (portal *Portal) markHandled(source *User, message *waProto.WebMessageInfo, mxid types.MatrixEventID) {
	msg := portal.bridge.DB.Message.New()
	msg.Chat = portal.Key
	msg.JID = message.GetKey().GetId()
	msg.MXID = mxid
	msg.Timestamp = message.GetMessageTimestamp()
	if message.GetKey().GetFromMe() {
		msg.Sender = source.JID
	} else if portal.IsPrivateChat() {
		msg.Sender = portal.Key.JID
	} else {
		msg.Sender = message.GetKey().GetParticipant()
		if len(msg.Sender) == 0 {
			msg.Sender = message.GetParticipant()
		}
	}
	msg.Content = message.Message
	msg.Insert()

	portal.recentlyHandledLock.Lock()
	index := portal.recentlyHandledIndex
	portal.recentlyHandledIndex = (portal.recentlyHandledIndex + 1) % recentlyHandledLength
	portal.recentlyHandledLock.Unlock()
	portal.recentlyHandled[index] = msg.JID
}

func (portal *Portal) startHandling(info whatsapp.MessageInfo) bool {
	if portal.lastMessageTs > info.Timestamp+1 ||
		portal.isRecentlyHandled(info.Id) ||
		portal.isDuplicate(info.Id) {
		return false
	}
	portal.lastMessageTs = info.Timestamp
	return true
}

func (portal *Portal) finishHandling(source *User, message *waProto.WebMessageInfo, mxid types.MatrixEventID) {
	portal.markHandled(source, message, mxid)
	portal.log.Debugln("Handled message", message.GetKey().GetId(), "->", mxid)
}

func (portal *Portal) SyncParticipants(metadata *whatsappExt.GroupInfo) {
	changed := false
	levels, err := portal.MainIntent().PowerLevels(portal.MXID)
	if err != nil {
		levels = portal.GetBasePowerLevels()
		changed = true
	}
	for _, participant := range metadata.Participants {
		user := portal.bridge.GetUserByJID(participant.JID)
		if user != nil && !portal.bridge.AS.StateStore.IsInvited(portal.MXID, user.MXID) {
			_, err = portal.MainIntent().InviteUser(portal.MXID, &mautrix.ReqInviteUser{
				UserID: user.MXID,
			})
			if err != nil {
				portal.log.Warnfln("Failed to invite %s to %s: %v", user.MXID, portal.MXID, err)
			}
		}

		puppet := portal.bridge.GetPuppetByJID(participant.JID)
		err := puppet.IntentFor(portal).EnsureJoined(portal.MXID)
		if err != nil {
			portal.log.Warnfln("Failed to make puppet of %s join %s: %v", participant.JID, portal.MXID, err)
		}

		expectedLevel := 0
		if participant.IsSuperAdmin {
			expectedLevel = 95
		} else if participant.IsAdmin {
			expectedLevel = 50
		}
		changed = levels.EnsureUserLevel(puppet.MXID, expectedLevel) || changed
		if user != nil {
			changed = levels.EnsureUserLevel(user.MXID, expectedLevel) || changed
		}
	}
	if changed {
		_, err = portal.MainIntent().SetPowerLevels(portal.MXID, levels)
		if err != nil {
			portal.log.Errorln("Failed to change power levels:", err)
		}
	}
}

func (portal *Portal) UpdateAvatar(user *User, avatar *whatsappExt.ProfilePicInfo) bool {
	if avatar == nil {
		var err error
		avatar, err = user.Conn.GetProfilePicThumb(portal.Key.JID)
		if err != nil {
			portal.log.Errorln(err)
			return false
		}
	}

	if avatar.Status != 0 {
		return false
	}

	if portal.Avatar == avatar.Tag {
		return false
	}

	data, err := avatar.DownloadBytes()
	if err != nil {
		portal.log.Warnln("Failed to download avatar:", err)
		return false
	}

	mimeType := http.DetectContentType(data)
	resp, err := portal.MainIntent().UploadBytes(data, mimeType)
	if err != nil {
		portal.log.Warnln("Failed to upload avatar:", err)
		return false
	}

	portal.AvatarURL = resp.ContentURI
	if len(portal.MXID) > 0 {
		_, err = portal.MainIntent().SetRoomAvatar(portal.MXID, resp.ContentURI)
		if err != nil {
			portal.log.Warnln("Failed to set room topic:", err)
			return false
		}
	}
	portal.Avatar = avatar.Tag
	return true
}

func (portal *Portal) UpdateName(name string, setBy types.WhatsAppID) bool {
	if portal.Name != name {
		intent := portal.MainIntent()
		if len(setBy) > 0 {
			intent = portal.bridge.GetPuppetByJID(setBy).IntentFor(portal)
		}
		_, err := intent.SetRoomName(portal.MXID, name)
		if err == nil {
			portal.Name = name
			return true
		}
		portal.log.Warnln("Failed to set room name:", err)
	}
	return false
}

func (portal *Portal) UpdateTopic(topic string, setBy types.WhatsAppID) bool {
	if portal.Topic != topic {
		intent := portal.MainIntent()
		if len(setBy) > 0 {
			intent = portal.bridge.GetPuppetByJID(setBy).IntentFor(portal)
		}
		_, err := intent.SetRoomTopic(portal.MXID, topic)
		if err == nil {
			portal.Topic = topic
			return true
		}
		portal.log.Warnln("Failed to set room topic:", err)
	}
	return false
}

func (portal *Portal) UpdateMetadata(user *User) bool {
	if portal.IsPrivateChat() {
		return false
	} else if portal.IsStatusBroadcastRoom() {
		update := false
		update = portal.UpdateName("WhatsApp Status Broadcast", "") || update
		update = portal.UpdateTopic("WhatsApp status updates from your contacts", "") || update
		return update
	}
	metadata, err := user.Conn.GetGroupMetaData(portal.Key.JID)
	if err != nil {
		portal.log.Errorln(err)
		return false
	}
	if metadata.Status != 0 {
		// 401: access denied
		// 404: group does (no longer) exist
		// 500: ??? happens with status@broadcast

		// TODO: update the room, e.g. change priority level
		//   to send messages to moderator
		return false
	}

	portal.SyncParticipants(metadata)
	update := false
	update = portal.UpdateName(metadata.Name, metadata.NameSetBy) || update
	update = portal.UpdateTopic(metadata.Topic, metadata.TopicSetBy) || update
	return update
}

func (portal *Portal) ensureUserInvited(user *User) {
	err := portal.MainIntent().EnsureInvited(portal.MXID, user.MXID)
	if err != nil {
		portal.log.Warnfln("Failed to ensure %s is invited to %s: %v", user.MXID, portal.MXID, err)
	}
	customPuppet := portal.bridge.GetPuppetByCustomMXID(user.MXID)
	if customPuppet != nil && customPuppet.CustomIntent() != nil {
		_ = customPuppet.CustomIntent().EnsureJoined(portal.MXID)
	}
}

func (portal *Portal) Sync(user *User, contact whatsapp.Contact) {
	portal.log.Infoln("Syncing portal for", user.MXID)

	if len(portal.MXID) == 0 {
		if !portal.IsPrivateChat() {
			portal.Name = contact.Name
		}
		err := portal.CreateMatrixRoom(user)
		if err != nil {
			portal.log.Errorln("Failed to create portal room:", err)
			return
		}
	} else {
		portal.ensureUserInvited(user)
	}

	if portal.IsPrivateChat() {
		return
	}

	update := false
	update = portal.UpdateMetadata(user) || update
	if !portal.IsStatusBroadcastRoom() {
		update = portal.UpdateAvatar(user, nil) || update
	}
	if update {
		portal.Update()
	}
}

func (portal *Portal) GetBasePowerLevels() *mautrix.PowerLevels {
	anyone := 0
	nope := 99
	return &mautrix.PowerLevels{
		UsersDefault:    anyone,
		EventsDefault:   anyone,
		RedactPtr:       &anyone,
		StateDefaultPtr: &nope,
		BanPtr:          &nope,
		InvitePtr:       &nope,
		Users: map[string]int{
			portal.MainIntent().UserID: 100,
		},
		Events: map[string]int{
			mautrix.StateRoomName.Type:   anyone,
			mautrix.StateRoomAvatar.Type: anyone,
			mautrix.StateTopic.Type:      anyone,
		},
	}
}

func (portal *Portal) ChangeAdminStatus(jids []string, setAdmin bool) {
	levels, err := portal.MainIntent().PowerLevels(portal.MXID)
	if err != nil {
		levels = portal.GetBasePowerLevels()
	}
	newLevel := 0
	if setAdmin {
		newLevel = 50
	}
	changed := false
	for _, jid := range jids {
		puppet := portal.bridge.GetPuppetByJID(jid)
		changed = levels.EnsureUserLevel(puppet.MXID, newLevel) || changed

		user := portal.bridge.GetUserByJID(jid)
		if user != nil {
			changed = levels.EnsureUserLevel(user.MXID, newLevel) || changed
		}
	}
	if changed {
		_, err = portal.MainIntent().SetPowerLevels(portal.MXID, levels)
		if err != nil {
			portal.log.Errorln("Failed to change power levels:", err)
		}
	}
}

func (portal *Portal) RestrictMessageSending(restrict bool) {
	levels, err := portal.MainIntent().PowerLevels(portal.MXID)
	if err != nil {
		levels = portal.GetBasePowerLevels()
	}
	if restrict {
		levels.EventsDefault = 50
	} else {
		levels.EventsDefault = 0
	}
	_, err = portal.MainIntent().SetPowerLevels(portal.MXID, levels)
	if err != nil {
		portal.log.Errorln("Failed to change power levels:", err)
	}
}

func (portal *Portal) RestrictMetadataChanges(restrict bool) {
	levels, err := portal.MainIntent().PowerLevels(portal.MXID)
	if err != nil {
		levels = portal.GetBasePowerLevels()
	}
	newLevel := 0
	if restrict {
		newLevel = 50
	}
	changed := false
	changed = levels.EnsureEventLevel(mautrix.StateRoomName, newLevel) || changed
	changed = levels.EnsureEventLevel(mautrix.StateRoomAvatar, newLevel) || changed
	changed = levels.EnsureEventLevel(mautrix.StateTopic, newLevel) || changed
	if changed {
		_, err = portal.MainIntent().SetPowerLevels(portal.MXID, levels)
		if err != nil {
			portal.log.Errorln("Failed to change power levels:", err)
		}
	}
}

func (portal *Portal) BackfillHistory(user *User, lastMessageTime uint64) error {
	if !portal.bridge.Config.Bridge.RecoverHistory {
		return nil
	}

	endBackfill := portal.beginBackfill()
	defer endBackfill()

	lastMessage := portal.bridge.DB.Message.GetLastInChat(portal.Key)
	if lastMessage == nil {
		return nil
	}
	if lastMessage.Timestamp >= lastMessageTime {
		portal.log.Debugln("Not backfilling: no new messages")
		return nil
	}

	lastMessageID := lastMessage.JID
	lastMessageFromMe := lastMessage.Sender == user.JID
	portal.log.Infoln("Backfilling history since", lastMessageID, "for", user.MXID)
	for len(lastMessageID) > 0 {
		portal.log.Debugln("Backfilling history: 50 messages after", lastMessageID)
		resp, err := user.Conn.LoadMessagesAfter(portal.Key.JID, lastMessageID, lastMessageFromMe, 50)
		if err != nil {
			return err
		}
		messages, ok := resp.Content.([]interface{})
		if !ok || len(messages) == 0 {
			break
		}

		portal.handleHistory(user, messages)

		lastMessageProto, ok := messages[len(messages)-1].(*waProto.WebMessageInfo)
		if ok {
			lastMessageID = lastMessageProto.GetKey().GetId()
			lastMessageFromMe = lastMessageProto.GetKey().GetFromMe()
		}
	}
	portal.log.Infoln("Backfilling finished")
	return nil
}

func (portal *Portal) beginBackfill() func() {
	portal.backfillLock.Lock()
	portal.backfilling = true
	var privateChatPuppetInvited bool
	var privateChatPuppet *Puppet
	if portal.IsPrivateChat() && portal.bridge.Config.Bridge.InviteOwnPuppetForBackfilling {
		privateChatPuppet = portal.bridge.GetPuppetByJID(portal.Key.Receiver)
		portal.privateChatBackfillInvitePuppet = func() {
			if privateChatPuppetInvited {
				return
			}
			privateChatPuppetInvited = true
			_, _ = portal.MainIntent().InviteUser(portal.MXID, &mautrix.ReqInviteUser{UserID: privateChatPuppet.MXID})
			_ = privateChatPuppet.DefaultIntent().EnsureJoined(portal.MXID)
		}
	}
	return func() {
		portal.backfilling = false
		portal.privateChatBackfillInvitePuppet = nil
		portal.backfillLock.Unlock()
		if privateChatPuppet != nil && privateChatPuppetInvited {
			_, _ = privateChatPuppet.DefaultIntent().LeaveRoom(portal.MXID)
		}
	}
}

func (portal *Portal) FillInitialHistory(user *User) error {
	if portal.bridge.Config.Bridge.InitialHistoryFill == 0 {
		return nil
	}
	endBackfill := portal.beginBackfill()
	defer endBackfill()
	if portal.privateChatBackfillInvitePuppet != nil {
		portal.privateChatBackfillInvitePuppet()
	}

	n := portal.bridge.Config.Bridge.InitialHistoryFill
	portal.log.Infoln("Filling initial history, maximum", n, "messages")
	var messages []interface{}
	before := ""
	fromMe := true
	chunkNum := 1
	for n > 0 {
		count := 50
		if n < count {
			count = n
		}
		portal.log.Debugfln("Fetching chunk %d (%d messages / %d cap) before message %s", chunkNum, count, n, before)
		resp, err := user.Conn.LoadMessagesBefore(portal.Key.JID, before, fromMe, count)
		if err != nil {
			return err
		}
		chunk, ok := resp.Content.([]interface{})
		if !ok || len(chunk) == 0 {
			portal.log.Infoln("Chunk empty, starting handling of loaded messages")
			break
		}

		messages = append(chunk, messages...)

		portal.log.Debugfln("Fetched chunk and received %d messages", len(chunk))

		n -= len(chunk)
		key := chunk[0].(*waProto.WebMessageInfo).GetKey()
		before = key.GetId()
		fromMe = key.GetFromMe()
		if len(before) == 0 {
			portal.log.Infoln("No message ID for first message, starting handling of loaded messages")
			break
		}
	}
	portal.handleHistory(user, messages)
	portal.log.Infoln("Initial history fill complete")
	return nil
}

func (portal *Portal) handleHistory(user *User, messages []interface{}) {
	portal.log.Infoln("Handling", len(messages), "messages of history")
	for _, rawMessage := range messages {
		message, ok := rawMessage.(*waProto.WebMessageInfo)
		if !ok {
			portal.log.Warnln("Unexpected non-WebMessageInfo item in history response:", rawMessage)
			continue
		}
		data := whatsapp.ParseProtoMessage(message)
		if portal.privateChatBackfillInvitePuppet != nil && message.GetKey().GetFromMe() && portal.IsPrivateChat() {
			portal.privateChatBackfillInvitePuppet()
		}
		portal.handleMessage(PortalMessage{portal.Key.JID, user, data, message.GetMessageTimestamp()})
	}
}

func (portal *Portal) CreateMatrixRoom(user *User) error {
	portal.roomCreateLock.Lock()
	defer portal.roomCreateLock.Unlock()
	if len(portal.MXID) > 0 {
		return nil
	}

	intent := portal.MainIntent()
	if err := intent.EnsureRegistered(); err != nil {
		return err
	}

	portal.log.Infoln("Creating Matrix room. Info source:", user.MXID)

	var metadata *whatsappExt.GroupInfo
	isPrivateChat := false
	if portal.IsPrivateChat() {
		puppet := portal.bridge.GetPuppetByJID(portal.Key.JID)
		if portal.bridge.Config.Bridge.PrivateChatPortalMeta {
			portal.Name = puppet.Displayname
			portal.AvatarURL = puppet.AvatarURL
			portal.Avatar = puppet.Avatar
		} else {
			portal.Name = ""
		}
		portal.Topic = "WhatsApp private chat"
		isPrivateChat = true
	} else if portal.IsStatusBroadcastRoom() {
		portal.Name = "WhatsApp Status Broadcast"
		portal.Topic = "WhatsApp status updates from your contacts"
	} else {
		var err error
		metadata, err = user.Conn.GetGroupMetaData(portal.Key.JID)
		if err == nil && metadata.Status == 0 {
			portal.Name = metadata.Name
			portal.Topic = metadata.Topic
		}
		portal.UpdateAvatar(user, nil)
	}

	initialState := []*mautrix.Event{{
		Type: mautrix.StatePowerLevels,
		Content: mautrix.Content{
			PowerLevels: portal.GetBasePowerLevels(),
		},
	}}
	if len(portal.AvatarURL) > 0 {
		initialState = append(initialState, &mautrix.Event{
			Type: mautrix.StateRoomAvatar,
			Content: mautrix.Content{
				URL: portal.AvatarURL,
			},
		})
	}

	resp, err := intent.CreateRoom(&mautrix.ReqCreateRoom{
		Visibility:   "private",
		Name:         portal.Name,
		Topic:        portal.Topic,
		Invite:       []string{user.MXID},
		Preset:       "private_chat",
		IsDirect:     isPrivateChat,
		InitialState: initialState,
	})
	if err != nil {
		return err
	}
	portal.MXID = resp.RoomID
	portal.Update()
	if metadata != nil {
		portal.SyncParticipants(metadata)
	} else {
		customPuppet := portal.bridge.GetPuppetByCustomMXID(user.MXID)
		if customPuppet != nil && customPuppet.CustomIntent() != nil {
			_ = customPuppet.CustomIntent().EnsureJoined(portal.MXID)
		}
	}
	err = portal.FillInitialHistory(user)
	if err != nil {
		portal.log.Errorln("Failed to fill history:", err)
	}
	return nil
}

func (portal *Portal) IsPrivateChat() bool {
	if portal.isPrivate == nil {
		val := strings.HasSuffix(portal.Key.JID, whatsappExt.NewUserSuffix)
		portal.isPrivate = &val
	}
	return *portal.isPrivate
}

func (portal *Portal) IsStatusBroadcastRoom() bool {
	return portal.Key.JID == "status@broadcast"
}

func (portal *Portal) MainIntent() *appservice.IntentAPI {
	if portal.IsPrivateChat() {
		return portal.bridge.GetPuppetByJID(portal.Key.JID).DefaultIntent()
	}
	return portal.bridge.Bot
}

func (portal *Portal) GetMessageIntent(user *User, info whatsapp.MessageInfo) *appservice.IntentAPI {
	if info.FromMe {
		return portal.bridge.GetPuppetByJID(user.JID).IntentFor(portal)
	} else if portal.IsPrivateChat() {
		return portal.MainIntent()
	} else if len(info.SenderJid) == 0 {
		if len(info.Source.GetParticipant()) != 0 {
			info.SenderJid = info.Source.GetParticipant()
		} else {
			return nil
		}
	}
	return portal.bridge.GetPuppetByJID(info.SenderJid).IntentFor(portal)
}

func (portal *Portal) SetReply(content *mautrix.Content, info whatsapp.MessageInfo) {
	if len(info.QuotedMessageID) == 0 {
		return
	}
	message := portal.bridge.DB.Message.GetByJID(portal.Key, info.QuotedMessageID)
	if message != nil {
		event, err := portal.MainIntent().GetEvent(portal.MXID, message.MXID)
		if err != nil {
			portal.log.Warnln("Failed to get reply target:", err)
			return
		}
		event.Content.RemoveReplyFallback()
		content.SetReply(event)
	}
	return
}

func (portal *Portal) HandleMessageRevoke(user *User, message whatsappExt.MessageRevocation) {
	msg := portal.bridge.DB.Message.GetByJID(portal.Key, message.Id)
	if msg == nil {
		return
	}
	var intent *appservice.IntentAPI
	if message.FromMe {
		if portal.IsPrivateChat() {
			intent = portal.bridge.GetPuppetByJID(user.JID).CustomIntent()
		} else {
			intent = portal.bridge.GetPuppetByJID(user.JID).IntentFor(portal)
		}
	} else if len(message.Participant) > 0 {
		intent = portal.bridge.GetPuppetByJID(message.Participant).IntentFor(portal)
	}
	if intent == nil {
		intent = portal.MainIntent()
	}
	_, err := intent.RedactEvent(portal.MXID, msg.MXID)
	if err != nil {
		portal.log.Errorln("Failed to redact %s: %v", msg.JID, err)
		return
	}
	msg.Delete()
}

func (portal *Portal) HandleFakeMessage(source *User, message FakeMessage) {
	if portal.isRecentlyHandled(message.ID) {
		return
	}

	_, err := portal.MainIntent().SendText(portal.MXID, message.Text)
	if err != nil {
		portal.log.Errorfln("Failed to handle fake message %s: %v", message.ID, err)
		return
	}

	portal.recentlyHandledLock.Lock()
	index := portal.recentlyHandledIndex
	portal.recentlyHandledIndex = (portal.recentlyHandledIndex + 1) % recentlyHandledLength
	portal.recentlyHandledLock.Unlock()
	portal.recentlyHandled[index] = message.ID
}

type MessageContent struct {
	*mautrix.Content
	IsCustomPuppet bool `json:"net.maunium.whatsapp.puppet,omitempty"`
}

func (portal *Portal) HandleTextMessage(source *User, message whatsapp.TextMessage) {
	if !portal.startHandling(message.Info) {
		return
	}

	intent := portal.GetMessageIntent(source, message.Info)
	if intent == nil {
		return
	}

	content := &mautrix.Content{
		Body:    message.Text,
		MsgType: mautrix.MsgText,
	}

	portal.bridge.Formatter.ParseWhatsApp(content)
	portal.SetReply(content, message.Info)

	_, _ = intent.UserTyping(portal.MXID, false, 0)
	resp, err := intent.SendMassagedMessageEvent(portal.MXID, mautrix.EventMessage, &MessageContent{content, intent.IsCustomPuppet}, int64(message.Info.Timestamp*1000))
	if err != nil {
		portal.log.Errorfln("Failed to handle message %s: %v", message.Info.Id, err)
		return
	}
	portal.finishHandling(source, message.Info.Source, resp.EventID)
}

func (portal *Portal) HandleMediaMessage(source *User, download func() ([]byte, error), thumbnail []byte, info whatsapp.MessageInfo, mimeType, caption string) {
	if !portal.startHandling(info) {
		return
	}

	intent := portal.GetMessageIntent(source, info)
	if intent == nil {
		return
	}

	data, err := download()
	if err != nil {
		portal.log.Errorfln("Failed to download media for %s: %v", info.Id, err)
		return
	}

	uploaded, err := intent.UploadBytes(data, mimeType)
	if err != nil {
		portal.log.Errorfln("Failed to upload media for %s: %v", err)
		return
	}

	fileName := info.Id
	exts, _ := mime.ExtensionsByType(mimeType)
	if exts != nil && len(exts) > 0 {
		fileName += exts[0]
	}

	content := &mautrix.Content{
		Body: fileName,
		URL:  uploaded.ContentURI,
		Info: &mautrix.FileInfo{
			Size:     len(data),
			MimeType: mimeType,
		},
	}
	portal.SetReply(content, info)

	if thumbnail != nil {
		thumbnailMime := http.DetectContentType(thumbnail)
		uploadedThumbnail, _ := intent.UploadBytes(thumbnail, thumbnailMime)
		if uploadedThumbnail != nil {
			content.Info.ThumbnailURL = uploadedThumbnail.ContentURI
			cfg, _, _ := image.DecodeConfig(bytes.NewReader(data))
			content.Info.ThumbnailInfo = &mautrix.FileInfo{
				Size:     len(thumbnail),
				Width:    cfg.Width,
				Height:   cfg.Height,
				MimeType: thumbnailMime,
			}
		}
	}

	switch strings.ToLower(strings.Split(mimeType, "/")[0]) {
	case "image":
		content.MsgType = mautrix.MsgImage
		cfg, _, _ := image.DecodeConfig(bytes.NewReader(data))
		content.Info.Width = cfg.Width
		content.Info.Height = cfg.Height
	case "video":
		content.MsgType = mautrix.MsgVideo
	case "audio":
		content.MsgType = mautrix.MsgAudio
	default:
		content.MsgType = mautrix.MsgFile
	}

	_, _ = intent.UserTyping(portal.MXID, false, 0)
	ts := int64(info.Timestamp * 1000)
	resp, err := intent.SendMassagedMessageEvent(portal.MXID, mautrix.EventMessage, &MessageContent{content, intent.IsCustomPuppet}, ts)
	if err != nil {
		portal.log.Errorfln("Failed to handle message %s: %v", info.Id, err)
		return
	}

	if len(caption) > 0 {
		captionContent := &mautrix.Content{
			Body:    caption,
			MsgType: mautrix.MsgNotice,
		}

		portal.bridge.Formatter.ParseWhatsApp(captionContent)

		_, err := intent.SendMassagedMessageEvent(portal.MXID, mautrix.EventMessage, &MessageContent{captionContent, intent.IsCustomPuppet}, ts)
		if err != nil {
			portal.log.Warnfln("Failed to handle caption of message %s: %v", info.Id, err)
		}
		// TODO store caption mxid?
	}

	portal.finishHandling(source, info.Source, resp.EventID)
}

func makeMessageID() *string {
	b := make([]byte, 10)
	rand.Read(b)
	str := strings.ToUpper(hex.EncodeToString(b))
	return &str
}

func (portal *Portal) downloadThumbnail(evt *mautrix.Event) []byte {
	if evt.Content.Info == nil || len(evt.Content.Info.ThumbnailURL) == 0 {
		return nil
	}

	thumbnail, err := portal.MainIntent().DownloadBytes(evt.Content.Info.ThumbnailURL)
	if err != nil {
		portal.log.Errorln("Failed to download thumbnail in %s: %v", evt.ID, err)
		return nil
	}
	thumbnailType := http.DetectContentType(thumbnail)
	var img image.Image
	switch thumbnailType {
	case "image/png":
		img, err = png.Decode(bytes.NewReader(thumbnail))
	case "image/gif":
		img, err = gif.Decode(bytes.NewReader(thumbnail))
	case "image/jpeg":
		return thumbnail
	default:
		return nil
	}
	var buf bytes.Buffer
	err = jpeg.Encode(&buf, img, &jpeg.Options{
		Quality: jpeg.DefaultQuality,
	})
	if err != nil {
		portal.log.Errorln("Failed to re-encode thumbnail in %s: %v", evt.ID, err)
		return nil
	}
	return buf.Bytes()
}

func (portal *Portal) preprocessMatrixMedia(sender *User, evt *mautrix.Event, mediaType whatsapp.MediaType) *MediaUpload {
	if evt.Content.Info == nil {
		evt.Content.Info = &mautrix.FileInfo{}
	}
	caption := evt.Content.Body
	exts, err := mime.ExtensionsByType(evt.Content.Info.MimeType)
	for _, ext := range exts {
		if strings.HasSuffix(caption, ext) {
			caption = ""
			break
		}
	}
	content, err := portal.MainIntent().DownloadBytes(evt.Content.URL)
	if err != nil {
		portal.log.Errorfln("Failed to download media in %s: %v", evt.ID, err)
		return nil
	}

	url, mediaKey, fileEncSHA256, fileSHA256, fileLength, err := sender.Conn.Upload(bytes.NewReader(content), mediaType)
	if err != nil {
		portal.log.Errorfln("Failed to upload media in %s: %v", evt.ID, err)
		return nil
	}

	return &MediaUpload{
		Caption:       caption,
		URL:           url,
		MediaKey:      mediaKey,
		FileEncSHA256: fileEncSHA256,
		FileSHA256:    fileSHA256,
		FileLength:    fileLength,
		Thumbnail:     portal.downloadThumbnail(evt),
	}
}

type MediaUpload struct {
	Caption       string
	URL           string
	MediaKey      []byte
	FileEncSHA256 []byte
	FileSHA256    []byte
	FileLength    uint64
	Thumbnail     []byte
}

func (portal *Portal) HandleMatrixMessage(sender *User, evt *mautrix.Event) {
	if portal.IsPrivateChat() && sender.JID != portal.Key.Receiver {
		return
	}
	portal.log.Debugfln("Received event %s", evt.ID)

	ts := uint64(evt.Timestamp / 1000)
	status := waProto.WebMessageInfo_ERROR
	fromMe := true
	info := &waProto.WebMessageInfo{
		Key: &waProto.MessageKey{
			FromMe:    &fromMe,
			Id:        makeMessageID(),
			RemoteJid: &portal.Key.JID,
		},
		MessageTimestamp: &ts,
		Message:          &waProto.Message{},
		Status:           &status,
	}
	ctxInfo := &waProto.ContextInfo{}
	replyToID := evt.Content.GetReplyTo()
	if len(replyToID) > 0 {
		evt.Content.RemoveReplyFallback()
		msg := portal.bridge.DB.Message.GetByMXID(replyToID)
		if msg != nil && msg.Content != nil {
			ctxInfo.StanzaId = &msg.JID
			ctxInfo.Participant = &msg.Sender
			ctxInfo.QuotedMessage = msg.Content
		}
	}
	var err error
	switch evt.Content.MsgType {
	case mautrix.MsgText, mautrix.MsgEmote:
		text := evt.Content.Body
		if evt.Content.Format == mautrix.FormatHTML {
			text = portal.bridge.Formatter.ParseMatrix(evt.Content.FormattedBody)
		}
		if evt.Content.MsgType == mautrix.MsgEmote {
			text = "/me " + text
		}
		ctxInfo.MentionedJid = mentionRegex.FindAllString(text, -1)
		for index, mention := range ctxInfo.MentionedJid {
			ctxInfo.MentionedJid[index] = mention[1:] + whatsappExt.NewUserSuffix
		}
		if ctxInfo.StanzaId != nil || ctxInfo.MentionedJid != nil {
			info.Message.ExtendedTextMessage = &waProto.ExtendedTextMessage{
				Text:        &text,
				ContextInfo: ctxInfo,
			}
		} else {
			info.Message.Conversation = &text
		}
	case mautrix.MsgImage:
		media := portal.preprocessMatrixMedia(sender, evt, whatsapp.MediaImage)
		if media == nil {
			return
		}
		info.Message.ImageMessage = &waProto.ImageMessage{
			Caption:       &media.Caption,
			JpegThumbnail: media.Thumbnail,
			Url:           &media.URL,
			MediaKey:      media.MediaKey,
			Mimetype:      &evt.Content.GetInfo().MimeType,
			FileEncSha256: media.FileEncSHA256,
			FileSha256:    media.FileSHA256,
			FileLength:    &media.FileLength,
		}
	case mautrix.MsgVideo:
		media := portal.preprocessMatrixMedia(sender, evt, whatsapp.MediaVideo)
		if media == nil {
			return
		}
		duration := uint32(evt.Content.GetInfo().Duration)
		info.Message.VideoMessage = &waProto.VideoMessage{
			Caption:       &media.Caption,
			JpegThumbnail: media.Thumbnail,
			Url:           &media.URL,
			MediaKey:      media.MediaKey,
			Mimetype:      &evt.Content.GetInfo().MimeType,
			Seconds:       &duration,
			FileEncSha256: media.FileEncSHA256,
			FileSha256:    media.FileSHA256,
			FileLength:    &media.FileLength,
		}
	case mautrix.MsgAudio:
		media := portal.preprocessMatrixMedia(sender, evt, whatsapp.MediaAudio)
		if media == nil {
			return
		}
		duration := uint32(evt.Content.GetInfo().Duration)
		info.Message.AudioMessage = &waProto.AudioMessage{
			Url:           &media.URL,
			MediaKey:      media.MediaKey,
			Mimetype:      &evt.Content.GetInfo().MimeType,
			Seconds:       &duration,
			FileEncSha256: media.FileEncSHA256,
			FileSha256:    media.FileSHA256,
			FileLength:    &media.FileLength,
		}
	case mautrix.MsgFile:
		media := portal.preprocessMatrixMedia(sender, evt, whatsapp.MediaDocument)
		if media == nil {
			return
		}
		info.Message.DocumentMessage = &waProto.DocumentMessage{
			Url:           &media.URL,
			MediaKey:      media.MediaKey,
			Mimetype:      &evt.Content.GetInfo().MimeType,
			FileEncSha256: media.FileEncSHA256,
			FileSha256:    media.FileSHA256,
			FileLength:    &media.FileLength,
		}
	default:
		portal.log.Debugln("Unhandled Matrix event:", evt)
		return
	}
	portal.markHandled(sender, info, evt.ID)
	portal.log.Debugln("Sending event", evt.ID, "to WhatsApp")
	_, err = sender.Conn.Send(info)
	if err != nil {
		portal.log.Errorfln("Error handling Matrix event %s: %v", evt.ID, err)
	} else {
		portal.log.Debugln("Handled Matrix event:", evt)
	}
}

func (portal *Portal) HandleMatrixRedaction(sender *User, evt *mautrix.Event) {
	if portal.IsPrivateChat() && sender.JID != portal.Key.Receiver {
		return
	}

	msg := portal.bridge.DB.Message.GetByMXID(evt.Redacts)
	if msg.Sender != sender.JID {
		return
	}

	ts := uint64(evt.Timestamp / 1000)
	status := waProto.WebMessageInfo_PENDING
	protoMsgType := waProto.ProtocolMessage_REVOKE
	fromMe := true
	info := &waProto.WebMessageInfo{
		Key: &waProto.MessageKey{
			FromMe:    &fromMe,
			Id:        makeMessageID(),
			RemoteJid: &portal.Key.JID,
		},
		MessageTimestamp: &ts,
		Message: &waProto.Message{
			ProtocolMessage: &waProto.ProtocolMessage{
				Type: &protoMsgType,
				Key: &waProto.MessageKey{
					FromMe:    &fromMe,
					Id:        &msg.JID,
					RemoteJid: &portal.Key.JID,
				},
			},
		},
		Status: &status,
	}
	_, err := sender.Conn.Send(info)
	if err != nil {
		portal.log.Errorfln("Error handling Matrix redaction: %s: %v", evt.ID, err)
	} else {
		portal.log.Debugln("Handled Matrix redaction:", evt)
	}
}

func (portal *Portal) Delete() {
	portal.Portal.Delete()
	delete(portal.bridge.portalsByJID, portal.Key)
	if len(portal.MXID) > 0 {
		delete(portal.bridge.portalsByMXID, portal.MXID)
	}
}

func (portal *Portal) Cleanup(puppetsOnly bool) {
	if len(portal.MXID) == 0 {
		return
	}
	if portal.IsPrivateChat() {
		_, err := portal.MainIntent().LeaveRoom(portal.MXID)
		if err != nil {
			portal.log.Warnln("Failed to leave private chat portal with main intent:", err)
		}
		return
	}
	intent := portal.MainIntent()
	members, err := intent.JoinedMembers(portal.MXID)
	if err != nil {
		portal.log.Errorln("Failed to get portal members for cleanup:", err)
		return
	}
	for member, _ := range members.Joined {
		if member == intent.UserID {
			continue
		}
		puppet := portal.bridge.GetPuppetByMXID(member)
		if puppet != nil {
			_, err = puppet.DefaultIntent().LeaveRoom(portal.MXID)
			if err != nil {
				portal.log.Errorln("Error leaving as puppet while cleaning up portal:", err)
			}
		} else if !puppetsOnly {
			_, err = intent.KickUser(portal.MXID, &mautrix.ReqKickUser{UserID: member, Reason: "Deleting portal"})
			if err != nil {
				portal.log.Errorln("Error kicking user while cleaning up portal:", err)
			}
		}
	}
	_, err = intent.LeaveRoom(portal.MXID)
	if err != nil {
		portal.log.Errorln("Error leaving with main intent while cleaning up portal:", err)
	}
}

func (portal *Portal) HandleMatrixLeave(sender *User) {
	if portal.IsPrivateChat() {
		portal.log.Debugln("User left private chat portal, cleaning up and deleting...")
		portal.Delete()
		portal.Cleanup(false)
		return
	}
}

func (portal *Portal) HandleMatrixKick(sender *User, event *mautrix.Event) {
	// TODO
}
