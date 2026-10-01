package scenario

import (
	"context"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/account"

	"github.com/idyl-labs/purlview/sdk/api"
)

// The fake platform keeps the live platform's sign-in rules, so commands built
// on it meet the same answers: attempts left, expiry on the fifth miss, a
// resend after a minute, three sends.
func TestFakeSignInFollowsThePlatformRules(t *testing.T) {
	t.Parallel()
	w := NewWorld()
	auth := authorizer{w.Platform}
	ctx := context.Background()
	start := func() *api.LoginChallenge {
		t.Helper()
		ch, err := auth.StartLogin(ctx, api.LoginStartRequest{Email: AccountEmail, DeviceLabel: ThisLabel})
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}
	ch := start()
	if !ch.ResendAt.Equal(Epoch.Add(time.Minute)) || !ch.ExpiresAt.Equal(Epoch.Add(5*time.Minute)) {
		t.Fatalf("challenge %+v", ch)
	}
	for left := 4; left >= 1; left-- {
		_, err := auth.VerifyLogin(ctx, api.LoginVerifyRequest{ID: ch.ID, Code: "000000"})
		if !account.IsKind(err, account.KindDenied) || account.AttemptsLeft(err) != left {
			t.Fatalf("miss with %d left: %v (%d)", left, err, account.AttemptsLeft(err))
		}
	}
	if _, err := auth.VerifyLogin(ctx, api.LoginVerifyRequest{ID: ch.ID, Code: "000000"}); !account.IsKind(err, account.KindExpired) || account.AttemptsLeft(err) != 0 {
		t.Fatalf("fifth miss: %v", err)
	}
	if _, err := auth.VerifyLogin(ctx, api.LoginVerifyRequest{ID: ch.ID, Code: w.Platform.LoginCode()}); !account.IsKind(err, account.KindExpired) {
		t.Fatalf("right code after five misses: %v", err)
	}

	ch = start()
	first := w.Platform.LoginCode()
	if _, err := auth.ResendLogin(ctx, api.LoginResendRequest{ID: ch.ID}); !account.IsKind(err, account.KindDenied) {
		t.Fatalf("resend before ResendAt: %v", err)
	}
	w.Clock.Advance(time.Minute)
	again, err := auth.ResendLogin(ctx, api.LoginResendRequest{ID: ch.ID})
	if err != nil || !again.ExpiresAt.Equal(ch.ExpiresAt) || !again.ResendAt.Equal(ch.ResendAt.Add(time.Minute)) || w.Platform.LoginCode() == first {
		t.Fatalf("resend: %+v %v", again, err)
	}
	if _, err = auth.VerifyLogin(ctx, api.LoginVerifyRequest{ID: ch.ID, Code: first}); account.AttemptsLeft(err) != 4 {
		t.Fatalf("the replaced code must miss: %v", err)
	}
	w.Clock.Advance(time.Minute)
	if _, err = auth.ResendLogin(ctx, api.LoginResendRequest{ID: ch.ID}); err != nil {
		t.Fatalf("second resend: %v", err)
	}
	w.Clock.Advance(time.Minute)
	if _, err = auth.ResendLogin(ctx, api.LoginResendRequest{ID: ch.ID}); !account.IsKind(err, account.KindDenied) {
		t.Fatalf("a fourth send: %v", err)
	}
	cred, err := auth.VerifyLogin(ctx, api.LoginVerifyRequest{ID: ch.ID, Code: w.Platform.LoginCode()})
	if err != nil || cred.Account != AccountEmail {
		t.Fatalf("verify: %+v %v", cred, err)
	}
	if _, err = auth.VerifyLogin(ctx, api.LoginVerifyRequest{ID: ch.ID, Code: w.Platform.LoginCode()}); !account.IsKind(err, account.KindExpired) {
		t.Fatalf("a used code: %v", err)
	}
	ch = start()
	w.Clock.Advance(5 * time.Minute)
	if _, err = auth.VerifyLogin(ctx, api.LoginVerifyRequest{ID: ch.ID, Code: w.Platform.LoginCode()}); !account.IsKind(err, account.KindExpired) {
		t.Fatalf("after five minutes: %v", err)
	}
}

func TestFakeInstallations(t *testing.T) {
	t.Parallel()
	w := NewWorld()
	var devices account.Installations = authorizer{w.Platform}
	ctx := context.Background()
	cred := w.SignIn()
	w.Platform.SeedRemoteShare("https://internal.example", 12*time.Minute, true)
	w.Platform.SeedRemoteShare("https://other.example", 12*time.Minute, false)
	list, err := devices.ListInstallations(ctx, cred)
	if err != nil || len(list) != 2 || list[0].ID != OtherDevice || list[0].ActiveShares != 2 || list[0].Current || list[1].ID != ThisDevice || !list[1].Current || list[1].ActiveShares != 0 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if (api.ListInstallationsResult{Installations: list}).Validate() != nil {
		t.Fatal("the fake must produce a valid wire result")
	}
	if _, err = devices.RevokeInstallation(ctx, cred, "dev_unknown"); !account.IsKind(err, account.KindNotFound) {
		t.Fatalf("unknown device: %v", err)
	}
	res, err := devices.RevokeInstallation(ctx, cred, OtherDevice)
	if err != nil || res.SharesStopped != 2 || res.AlreadyEnded || res.Outcome != "confirmed" || res.Validate() != nil {
		t.Fatalf("revoke: %+v %v", res, err)
	}
	res, err = devices.RevokeInstallation(ctx, cred, OtherDevice)
	if err != nil || res.SharesStopped != 0 || !res.AlreadyEnded {
		t.Fatalf("repeated revoke: %+v %v", res, err)
	}
	if list, err = devices.ListInstallations(ctx, cred); err != nil || len(list) != 1 || !list[0].Current {
		t.Fatalf("list after revoke: %+v %v", list, err)
	}
	w.Platform.Unreachable()
	if _, err = devices.ListInstallations(ctx, cred); !account.IsKind(err, account.KindUnavailable) {
		t.Fatalf("unreachable: %v", err)
	}
}

func TestFakeInvitesAreRecordedAndReplayed(t *testing.T) {
	t.Parallel()
	w := NewWorld()
	cred := w.SignIn()
	w.Platform.SendInvites("raj@example.invalid")
	req := api.CreateShareRequest{Key: "attempt", Target: "http://localhost:3000", TTL: time.Hour, Recipients: []string{"ana@example.invalid", "raj@example.invalid"}}
	first, err := w.Platform.createShare(context.Background(), *cred, req)
	if err != nil || first.Validate() != nil || len(first.Invites) != 2 || first.Invites[0].Status != api.InviteSent || first.Invites[1].Status != api.InviteNotSent {
		t.Fatalf("create: %+v %v", first, err)
	}
	mail := w.Platform.mail()
	if len(mail) != 1 || mail[0].To != "ana@example.invalid" || !mail[0].ExpiresAt.Equal(first.Share.ExpiresAt) {
		t.Fatalf("an invite lasts as long as the share: %+v", mail)
	}
	again, err := w.Platform.createShare(context.Background(), *cred, req)
	if err != nil || len(again.Invites) != 2 || again.Invites[1] != first.Invites[1] || len(w.Platform.mail()) != 1 {
		t.Fatalf("a replay reports the recorded results and sends nothing: %+v %v", again, err)
	}
}
