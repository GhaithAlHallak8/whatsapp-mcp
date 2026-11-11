package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func TestGroupMembersFromInfo(t *testing.T) {
	info := &types.GroupInfo{
		GroupName: types.GroupName{Name: "Cabinet"},
		Participants: []types.GroupParticipant{
			{JID: types.NewJID("111", types.HiddenUserServer), PhoneNumber: types.NewJID("971500000001", types.DefaultUserServer), LID: types.NewJID("111", types.HiddenUserServer), IsSuperAdmin: true},
			{JID: types.NewJID("971500000002", types.DefaultUserServer)},
			{JID: types.NewJID("333", types.HiddenUserServer)},
		},
	}
	resp := groupMembersFromInfo(info)
	if !resp.Success || resp.Name != "Cabinet" || len(resp.Members) != 3 {
		t.Fatalf("resp = %+v", resp)
	}
	first, second, third := resp.Members[0], resp.Members[1], resp.Members[2]
	if first.Phone != "971500000001" || first.LID != "111" || !first.IsAdmin || !first.IsSuperAdmin {
		t.Fatalf("first = %+v", first)
	}
	if second.Phone != "971500000002" || second.LID != "" || second.IsAdmin {
		t.Fatalf("second = %+v", second)
	}
	if third.Phone != "" || third.LID != "333" {
		t.Fatalf("third = %+v", third)
	}
}

func TestGroupMembersEndpointValidation(t *testing.T) {
	mux := http.NewServeMux()
	identity := func(h http.HandlerFunc) http.HandlerFunc { return h }
	registerGroupMembersEndpoint(mux, identity, nil)
	for name, tc := range map[string]struct {
		method, body string
		want         int
	}{
		"get":         {http.MethodGet, "", http.StatusMethodNotAllowed},
		"bad json":    {http.MethodPost, "{", http.StatusBadRequest},
		"not a group": {http.MethodPost, `{"chat_jid":"123@s.whatsapp.net"}`, http.StatusBadRequest},
		"offline":     {http.MethodPost, `{"chat_jid":"123@g.us"}`, http.StatusServiceUnavailable},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, "/api/group-members", strings.NewReader(tc.body)))
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", name, rec.Code, tc.want)
		}
		if tc.want == http.StatusServiceUnavailable {
			var resp GroupMembersResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Success || resp.Members == nil {
				t.Errorf("%s: body = %s", name, rec.Body.String())
			}
		}
	}
}

func TestHistoryAnchorCanStartFromAChosenMessage(t *testing.T) {
	ms := newTestMessageStore(t)
	chat := phonePN.String()
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	if err := ms.StoreChat(chat, "T", base); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"old", "mid", "new"} {
		if err := ms.StoreMessage(id, chat, "1", "x", base.Add(time.Duration(i)*time.Hour), id == "mid", "", "", "", nil, nil, nil, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	id, fromMe, ts, err := historyAnchor(ms, HistoryRequest{ChatJID: chat})
	if err != nil || id != "old" || fromMe || !ts.Equal(base) {
		t.Fatalf("default anchor = %q %v %v %v", id, fromMe, ts, err)
	}
	id, fromMe, ts, err = historyAnchor(ms, HistoryRequest{ChatJID: chat, BeforeMessageID: "mid"})
	if err != nil || id != "mid" || !fromMe || !ts.Equal(base.Add(time.Hour)) {
		t.Fatalf("chosen anchor = %q %v %v %v", id, fromMe, ts, err)
	}
	if _, _, _, err := historyAnchor(ms, HistoryRequest{ChatJID: chat, BeforeMessageID: "missing"}); err == nil {
		t.Fatal("unknown anchor message was accepted")
	}
}
