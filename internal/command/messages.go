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

import "github.com/idyl-labs/purlview/internal/output"

// The lines of the share session and of stop, as data. Handlers and the
// goldens use the same functions, so a golden pins what the command prints.

func firstVisitor() output.Message { return output.Msg(output.Live, "First visitor opened the app") }

func appUnresponsive(app string) output.Message {
	return output.Msg(output.Attend, output.Name(app), " stopped responding").Remedy("visitors see a waiting page")
}

func appResponding(app string) output.Message {
	return output.Msg(output.Done, output.Name(app), " is responding again")
}

func connectionLost() output.Message {
	return output.Msg(output.Attend, "Connection lost").Remedy("reconnecting")
}

func reconnected(expiresAt string) output.Message {
	return output.Msg(output.Done, "Reconnected").Remedy("same link, still expires " + expiresAt)
}

func expiresSoon() output.Message { return output.Msg(output.Attend, "Expires in 5 minutes") }

// shareExpired and stoppedElsewhere are orderly endings: no symbol, no
// colour.
func shareExpired() output.Message { return output.Msg(output.None, "Share expired") }

func stoppedElsewhere() output.Message { return output.Msg(output.None, "Stopped from another device") }

func stopped() output.Message {
	return output.Msg(output.Done, "Stopped").Remedy("the link no longer works")
}

// stoppedHere: nothing serves the share any more, but Purlview still lists
// it until it expires.
func stoppedHere(by string) output.Message {
	return output.Msg(output.Attend, "Stopped here, but Purlview couldn't confirm").Remedy("the link stops working by " + by)
}

func backgroundStopped(linkWorks bool) output.Message {
	m := output.Msg(output.Failure, "Purlview's background process stopped")
	if linkWorks {
		return m.Remedy("nothing was left running; try again")
	}
	return m.Remedy("the link no longer works")
}

func purlviewStoppedShare() output.Message {
	return output.Msg(output.Failure, "Purlview stopped this share").Remedy("the link no longer works")
}

func stoppedShare(id string) output.Message {
	return output.Msg(output.Done, "Stopped ", output.Name(id)).Remedy("the link no longer works")
}

func alreadyStopped(id string) output.Message {
	return output.Msg(output.Done, output.Name(id), " was already stopped")
}

func codeExpired() output.Message {
	return output.Msg(output.Failure, "That code has expired").Remedy("run ", bright("purlview login"), " to get a new one")
}

func signedOutHere() output.Message {
	return output.Msg(output.Done, "Signed out on this device").Remedy("running shares continue until stopped or expired")
}
