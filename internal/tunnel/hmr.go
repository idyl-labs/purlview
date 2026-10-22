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

package tunnel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// The frameworks whose hot reload the daemon helps pages catch up with. A
// framework belongs here only once it is known to meet the two requirements
// the catch-up's correctness rests on:
//
//   - its server sends each message to every client from one in-order loop,
//     so a message sent before a client registered reached none of the
//     sockets registered after it, and one sent after reached them all;
//   - it answers a WebSocket ping in order with the messages before it, so
//     a pong proves that every earlier message has been delivered.
//
// Vite 8 meets both: its server broadcasts from one loop over its clients,
// and the ws library writes the pong into the same ordered socket stream.
// Frameworks built on Vite's dev server (Astro, SvelteKit, Nuxt, React
// Router) use its HMR server and therefore its socket.

// framework describes one framework's HMR socket and messages.
type framework struct {
	name string
	// subprotocol recognises the framework's HMR socket, in the daemon and in
	// the page's script.
	subprotocol string
	// changes are the message types that change what a page shows; each one
	// the observer receives advances the target's revision.
	changes map[string]bool
	// errorType is the message that puts the app in error, replayed to new
	// visitor sockets until a change arrives.
	errorType string
	// clientPath is fetched from the app to find the HMR endpoint. A page
	// whose head names it, under any base, loads the framework's client.
	clientPath string
	// endpoint reads the socket's path and query from the client module at
	// clientPath; ok is false when the module is not this framework's.
	// elsewhere is true when the page dials another port for the socket, so
	// the app's own port may not serve it.
	endpoint func(module []byte) (uri string, elsewhere, ok bool)
}

var vite = &framework{
	name:        "Vite",
	subprotocol: "vite-hmr",
	changes:     map[string]bool{"full-reload": true, "update": true},
	errorType:   "error",
	clientPath:  "/@vite/client",
	endpoint:    viteEndpoint,
}

// frameworks is the supported table, tried in order.
var frameworks = []*framework{vite}

// Vite's served client module has its configuration substituted as literals
// (vite/dist/client/client.mjs, served at /@vite/client):
//
//	const socketHost = `${null || importMetaUrl.hostname}:${hmrPort || importMetaUrl.port}${"/"}`;
//	const wsToken = "ATaII4SM6oKN";
//
// The last literal of socketHost is the socket's path as the page uses it:
// the dev server's base joined with server.hmr.path. The observer connects
// to that path on the app's own port. A non-null hmrPort (server.hmr.port or
// clientPort) means the page dials another port: with clientPort set to the
// port the page is served on, the socket is still the app's own; with a
// separate HMR server, neither the observer nor a page through the share can
// reach it.
var (
	viteSocketHost = regexp.MustCompile(`(?m)^const socketHost = ` + "`" + `.*\$\{("(?:[^"\\]|\\.)*")\}` + "`" + `;`)
	viteToken      = regexp.MustCompile(`(?m)^const wsToken = ("(?:[^"\\]|\\.)*");`)
	viteHMRPort    = regexp.MustCompile(`(?m)^const hmrPort = ([^;]*);`)
)

// viteEndpoint reads the HMR socket's path and token from Vite's client
// module.
func viteEndpoint(module []byte) (uri string, elsewhere, ok bool) {
	m := viteSocketHost.FindSubmatch(module)
	if m == nil {
		return "", false, false
	}
	var path string
	if json.Unmarshal(m[1], &path) != nil || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "?#\\") {
		return "", false, false
	}
	if p := viteHMRPort.FindSubmatch(module); p != nil && strings.TrimSpace(string(p[1])) != "null" {
		elsewhere = true
	}
	uri = path
	if m := viteToken.FindSubmatch(module); m != nil {
		var token string
		if json.Unmarshal(m[1], &token) == nil && token != "" {
			uri += "?token=" + url.QueryEscape(token)
		}
	}
	return uri, elsewhere, true
}

// loadsClient finds the supported framework whose client a page's head
// loads, by the client's path, or nil.
func loadsClient(head []byte) *framework {
	for _, fw := range frameworks {
		if bytes.Contains(head, []byte(fw.clientPath)) {
			return fw
		}
	}
	return nil
}

// hmrUpgrade recognises a WebSocket upgrade for a supported framework's HMR
// socket by the subprotocols it offers.
func hmrUpgrade(r *http.Request) *framework {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil
	}
	for _, v := range r.Header.Values("Sec-Websocket-Protocol") {
		for _, p := range strings.Split(v, ",") {
			for _, fw := range frameworks {
				if strings.TrimSpace(p) == fw.subprotocol {
					return fw
				}
			}
		}
	}
	return nil
}

// messageType reads the type of a framework message: a JSON object with a
// string "type". Anything else has no type.
func messageType(msg []byte) string {
	var m struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return ""
	}
	return m.Type
}
