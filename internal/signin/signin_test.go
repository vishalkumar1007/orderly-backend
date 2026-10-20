package signin

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/internal/storefront"
)

func TestMethodsNeedTheirChannel(t *testing.T) {
	sf := &storefront.Storefront{CustomerLoginMethods: []string{storefront.LoginMethodPhone, storefront.LoginMethodEmail}}
	ctx := context.Background()
	tid := uuid.New()

	// Nothing set up: nothing is offered, whatever the owner chose.
	none := &Service{}
	if got := none.Offered(ctx, tid, sf); len(got) != 0 {
		t.Fatalf("with no channels set up, offered %v", got)
	}
	if ch := none.Channels(ctx, tid)[storefront.LoginMethodPhone]; ch.Ready || ch.Reason == "" {
		t.Fatalf("an unset channel should say why it is not ready: %+v", ch)
	}

	// The test fallback never survives production.
	if NewService(nil, nil, true, true).fallback {
		t.Fatal("the test fallback was honoured in production")
	}

	// Ready channels: only the owner's choice is offered, in a stable order.
	ready := &Service{fallback: true}
	sf.CustomerLoginMethods = []string{storefront.LoginMethodEmail}
	if got := ready.Offered(ctx, tid, sf); len(got) != 1 || got[0] != storefront.LoginMethodEmail {
		t.Fatalf("offered %v, want email only", got)
	}
	if got := storefront.LoginMethods([]string{"email", "PHONE", "sms", "EMAIL"}); len(got) != 2 || got[0] != "PHONE" {
		t.Fatalf("LoginMethods = %v", got)
	}
}
