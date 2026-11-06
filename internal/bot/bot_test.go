package bot

import (
	"context"
	"fmt"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// onMessage("login <code>") must bind the sender's open_id to the
// pairing state before the reply attempt (which fails offline).
func TestLoginPairingFlow(t *testing.T) {
	store := NewLoginStore()
	st := store.Create()
	b := New("id", "secret", "", lark.NewClient("x", "y"), "sign-secret", nil, store)

	ev := msgEvent("ou_testuser", "login "+st.Code)
	_ = b.onMessage(context.Background(), ev) // reply errors offline — fine

	got, ok := store.Redeem(st.Code)
	if !ok || !got.Issued {
		t.Fatal("pairing code was not issued")
	}
	if got.Label != "ou_testuser" {
		t.Fatalf("label = %q, want sender open_id", got.Label)
	}
	if got.Token == "" {
		t.Fatal("empty token")
	}
	if _, again := store.Redeem(st.Code); again {
		t.Fatal("redeem must be single-use")
	}
}

func TestUnknownCode(t *testing.T) {
	store := NewLoginStore()
	b := New("id", "secret", "", nil, "s", nil, store)
	_ = b.onMessage(context.Background(), msgEvent("ou_x", "login ZZZZZZ"))
	if _, ok := store.Get("ZZZZZZ"); ok {
		t.Fatal("unknown code must not create state")
	}
}

func msgEvent(openID, text string) *larkim.P2MessageReceiveV1 {
	ty, id, uid, content := "text", "om_x", openID,
		fmt.Sprintf(`{"text":%q}`, text)
	st := "user"
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId:   &larkim.UserId{OpenId: &uid},
				SenderType: &st,
			},
			Message: &larkim.EventMessage{
				MessageId:   &id,
				MessageType: &ty,
				Content:     &content,
			},
		},
	}
}
