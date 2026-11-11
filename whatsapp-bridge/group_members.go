package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

type GroupMember struct {
	JID          string `json:"jid"`
	Phone        string `json:"phone,omitempty"`
	LID          string `json:"lid,omitempty"`
	IsAdmin      bool   `json:"is_admin"`
	IsSuperAdmin bool   `json:"is_super_admin"`
	DisplayName  string `json:"display_name,omitempty"`
}

type GroupMembersResponse struct {
	Success bool          `json:"success"`
	Message string        `json:"message,omitempty"`
	Name    string        `json:"name,omitempty"`
	Topic   string        `json:"topic,omitempty"`
	Members []GroupMember `json:"members"`
}

var fetchGroupInfo = func(client *whatsmeow.Client, jid types.JID) (*types.GroupInfo, error) {
	return client.GetGroupInfo(context.Background(), jid)
}

func groupMembersFromInfo(info *types.GroupInfo) GroupMembersResponse {
	resp := GroupMembersResponse{Success: true, Name: info.Name, Topic: info.Topic, Members: []GroupMember{}}
	for _, participant := range info.Participants {
		member := GroupMember{
			JID:          participant.JID.ToNonAD().String(),
			IsAdmin:      participant.IsAdmin || participant.IsSuperAdmin,
			IsSuperAdmin: participant.IsSuperAdmin,
			DisplayName:  participant.DisplayName,
		}
		if !participant.PhoneNumber.IsEmpty() {
			member.Phone = participant.PhoneNumber.User
		} else if participant.JID.Server == types.DefaultUserServer {
			member.Phone = participant.JID.User
		}
		if !participant.LID.IsEmpty() {
			member.LID = participant.LID.User
		} else if participant.JID.Server == types.HiddenUserServer {
			member.LID = participant.JID.User
		}
		resp.Members = append(resp.Members, member)
	}
	return resp
}

func registerGroupMembersEndpoint(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, client *whatsmeow.Client) {
	mux.HandleFunc("/api/group-members", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ChatJID string `json:"chat_jid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fail := func(status int, message string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(GroupMembersResponse{Message: message, Members: []GroupMember{}})
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil || jid.Server != types.GroupServer {
			fail(http.StatusBadRequest, "chat_jid must be a group JID ending in @g.us")
			return
		}
		if client == nil || !client.IsConnected() {
			fail(http.StatusServiceUnavailable, "Not connected to WhatsApp")
			return
		}
		info, err := fetchGroupInfo(client, jid)
		if err != nil {
			fail(http.StatusBadGateway, fmt.Sprintf("Failed to fetch group info: %v", err))
			return
		}
		_ = json.NewEncoder(w).Encode(groupMembersFromInfo(info))
	}))
}
