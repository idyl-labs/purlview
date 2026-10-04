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

package ipc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func testPipeName(t *testing.T) string {
	t.Helper()
	var b [6]byte
	_, _ = rand.Read(b[:])
	return `\\.\pipe\purlview-test-` + hex.EncodeToString(b[:])
}

// The pipe must carry an explicit, protected DACL with exactly one allow
// entry for the current user, and be owned by that user; the client must be
// able to verify the owner. The check reads the ACEs rather than SDDL text,
// because Windows renders well-known accounts (the built-in Administrator,
// for one) as aliases such as "LA" and normalises the access mask.
func TestPipeSecurityDescriptorIsOwnerOnly(t *testing.T) {
	t.Parallel()
	name := testPipeName(t)
	l, err := Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := Dial(ctx, name)
	if err != nil {
		t.Fatalf("dial with owner verification: %v", err)
	}
	defer func() { _ = c.Close() }()
	h := windows.Handle(c.(interface{ Fd() uintptr }).Fd())
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pipe descriptor: %s", sd.String())
	sidText, err := CurrentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	me, err := windows.StringToSid(sidText)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil || !owner.Equals(me) {
		t.Fatalf("owner %v (%v), want %s", owner, err, sidText)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 || control&windows.SE_DACL_PRESENT == 0 {
		t.Fatalf("DACL must be present and protected from inheritance: control=%#x err=%v", control, err)
	}
	dacl, defaulted, err := sd.DACL()
	if err != nil || dacl == nil || defaulted {
		t.Fatalf("explicit DACL expected: %v %v", err, defaulted)
	}
	if dacl.AceCount != 1 {
		t.Fatalf("DACL has %d entries, want exactly one", dacl.AceCount)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		t.Fatalf("ACE type %d, want access-allowed", ace.Header.AceType)
	}
	if sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)); !sid.Equals(me) {
		t.Fatalf("ACE grants %s, want %s", sid.String(), sidText)
	}
	if ace.Mask&windows.GENERIC_ALL == 0 && ace.Mask&(windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE) != windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE {
		t.Fatalf("ACE mask %#x does not grant full access", ace.Mask)
	}
	// Remote clients are rejected at creation (FILE_PIPE_REJECT_REMOTE_CLIENTS
	// in go-winio's makeServerPipeHandle); confirm the server end reports
	// the client as a local process.
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(h, &pid); err == nil && pid != windows.GetCurrentProcessId() {
		t.Fatalf("client pid %d, want this process", pid)
	}
}
