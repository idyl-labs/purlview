// Copyright 2026 Idyl Labs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/daemon"
	"github.com/idyl-labs/purlview/internal/ipc"
	"github.com/idyl-labs/purlview/internal/share"
)

// daemonRunner is the production share.Runner: it starts or reuses the
// real per-user daemon and speaks the private IPC. The daemon answers
// share operations with not_implemented until it has a serving tunnel client;
// the runner maps that to share.ErrNotImplemented so commands fail
// honestly.
type daemonRunner struct {
	getenv func(string) string
	info   buildinfo.Info
	// replaced says that an outdated daemon, and its shares, had to go.
	replaced func()
}

func (r *daemonRunner) Open(ctx context.Context, start bool) (share.Session, error) {
	ctrl, err := daemon.NewController(r.getenv, r.info)
	if err != nil {
		return nil, err
	}
	ctrl.Replaced = r.replaced
	var sess *daemon.Session
	if start {
		sess, err = ctrl.Ensure(ctx)
	} else {
		sess, err = ctrl.Connect(ctx)
		if errors.Is(err, daemon.ErrNotRunning) {
			return nil, share.ErrDaemonNotRunning
		}
	}
	if err != nil {
		return nil, err
	}
	if err := sess.Client.Call(ctx, ipc.OpSubscribe, nil, nil); err != nil {
		_ = sess.Close()
		return nil, err
	}
	s := &ipcSession{sess: sess, events: make(chan share.Event, 64)}
	go s.pump()
	return s, nil
}

type ipcSession struct {
	sess   *daemon.Session
	events chan share.Event
}

func (s *ipcSession) pump() {
	defer close(s.events)
	for ev := range s.sess.Client.Events() {
		if ev.Event != ipc.EventShare {
			continue
		}
		var data ipc.ShareEventData
		if json.Unmarshal(ev.Data, &data) != nil {
			continue
		}
		s.events <- share.Event{Kind: share.EventKind(data.Kind), Share: fromRecord(data.Share), Remote: fromRemote(data.Remote), Detail: data.Detail}
	}
}

func mapIPCError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case ipc.IsCode(err, ipc.CodeNotImplemented):
		return share.ErrNotImplemented
	case ipc.IsCode(err, ipc.CodeNotFound):
		var e *ipc.Error
		_ = errors.As(err, &e)
		return &share.Failure{Kind: share.KindNotFound, Detail: e.Message}
	case ipc.IsCode(err, ipc.CodeShuttingDown):
		return fmt.Errorf("%w: the daemon is shutting down", share.ErrSessionClosed)
	}
	return err
}

func (s *ipcSession) Start(ctx context.Context, req share.StartRequest) (share.Share, error) {
	targets := share.TargetURLs(req.Spec.Targets)
	params := ipc.ShareStartParams{
		Attempt: req.Attempt, Owner: string(req.Owner), Target: targets[0], TTLMs: req.Spec.TTL.Milliseconds(),
		Recipients: req.Spec.Recipients, NoRewrite: req.Spec.NoRewrite,
		Credential: ipc.ShareCredential{Account: req.Credential.Account, AccountID: req.Credential.AccountID, Device: req.Credential.Device, DeviceLabel: req.Credential.DeviceLabel, Token: req.Credential.Token},
	}
	if len(targets) > 1 {
		params.Targets = targets
	}
	var rec ipc.ShareRecord
	if err := s.sess.Client.Call(ctx, ipc.OpShareStart, params, &rec); err != nil {
		return share.Share{}, mapIPCError(err)
	}
	return fromRecord(rec), nil
}

func (s *ipcSession) Events() <-chan share.Event { return s.events }

func (s *ipcSession) Stop(ctx context.Context, req share.StopRequest) (share.StopResult, error) {
	var res ipc.ShareStopResult
	if err := s.sess.Client.Call(ctx, ipc.OpShareStop, ipc.ShareStopParams{ID: req.ID, Attempt: req.Attempt, Reason: string(req.Reason)}, &res); err != nil {
		return share.StopResult{}, mapIPCError(err)
	}
	return share.StopResult{Share: fromRecord(res.Share), AlreadyEnded: res.AlreadyEnded, Remote: fromRemote(res.Remote)}, nil
}

func (s *ipcSession) StopAll(ctx context.Context, reason share.EndReason) (int, error) {
	var res ipc.ShareStopAllResult
	if err := s.sess.Client.Call(ctx, ipc.OpShareStopAll, ipc.ShareStopAllParams{Reason: string(reason)}, &res); err != nil {
		return 0, mapIPCError(err)
	}
	return res.Ended, nil
}

func (s *ipcSession) Shares(ctx context.Context) ([]share.Share, error) {
	var res ipc.ShareListResult
	if err := s.sess.Client.Call(ctx, ipc.OpShareList, nil, &res); err != nil {
		return nil, mapIPCError(err)
	}
	out := make([]share.Share, 0, len(res.Shares))
	for _, r := range res.Shares {
		out = append(out, fromRecord(r))
	}
	return out, nil
}

func (s *ipcSession) CheckUpdate(ctx context.Context, channel string, wait time.Duration) (*share.Release, error) {
	var res ipc.UpdateCheckResult
	if err := s.sess.Client.Call(ctx, ipc.OpUpdateCheck, ipc.UpdateCheckParams{Channel: channel, WaitMs: wait.Milliseconds()}, &res); err != nil {
		return nil, err
	}
	if res.Latest == nil {
		return nil, nil
	}
	return &share.Release{Version: res.Latest.Version, Tag: res.Latest.Tag, Prerelease: res.Latest.Prerelease, URL: res.Latest.URL}, nil
}

func (s *ipcSession) Close() error { return s.sess.Close() }

func fromRecord(r ipc.ShareRecord) share.Share {
	parse := func(v string) time.Time {
		t, _ := time.Parse(time.RFC3339Nano, v)
		return t
	}
	var invites []share.Invite
	for _, i := range r.Invites {
		invites = append(invites, share.Invite{Email: i.Email, Status: i.Status})
	}
	return share.Share{
		ID: r.ID, Origin: r.Origin, URL: r.URL, Attempt: r.Attempt, Target: r.Target, Targets: r.Targets, Device: r.Device, DeviceLabel: r.DeviceLabel,
		Recipients: r.Recipients, Invites: invites, RewriteURLs: r.RewriteURLs, NoRewrite: r.NoRewrite, Owner: share.Owner(r.Owner), State: share.State(r.State),
		CreatedAt: parse(r.CreatedAt), ExpiresAt: parse(r.ExpiresAt), EndReason: share.EndReason(r.EndReason), EndApp: r.EndApp, EndLimit: r.EndLimit, Detail: r.Detail,
	}
}

func fromRemote(r ipc.ShareRemote) share.RemoteOutcome {
	status := share.RemoteStatus(r.Status)
	if status == "" {
		status = share.RemoteNone
	}
	return share.RemoteOutcome{Status: status, Detail: r.Detail}
}
