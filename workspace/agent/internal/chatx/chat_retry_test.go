package chatx

import "testing"

func TestReusesFailedUserTurn(t *testing.T) {
	failed := &ChatConversation{Messages: []ChatMessage{{Role: "assistant", Content: "hi"}, {Role: "user", Content: "do it"}}}
	answered := &ChatConversation{Messages: []ChatMessage{{Role: "user", Content: "do it"}, {Role: "assistant", Content: "ok"}}}
	cases := []struct {
		name string
		c    *ChatConversation
		req  chatSendReq
		text string
		want bool
	}{
		{"retry of the stored failed turn", failed, chatSendReq{Retry: true}, "do it", true},
		{"plain resend always appends", failed, chatSendReq{}, "do it", false},
		{"retry with different text appends", failed, chatSendReq{Retry: true}, "other", false},
		{"retry after an answer appends", answered, chatSendReq{Retry: true}, "do it", false},
		{"retry on empty conversation", &ChatConversation{}, chatSendReq{Retry: true}, "do it", false},
	}
	for _, tc := range cases {
		if got := reusesFailedUserTurn(tc.c, tc.req, tc.text); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
