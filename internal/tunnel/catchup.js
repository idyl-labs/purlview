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

// The catch-up script, as readable source. The daemon inserts the minified
// form, catchupScript in catchup.go, into a shared page's HTML before any of
// the page's scripts that could run before it, followed by the page's stamp
// as its argument:
//
//   <script data-purlview="catch-up">(function (stamp) {...})({"g":"7","r":3,"u":"/","p":["vite-hmr"]})</script>
//
//   g  the observer generation the page was stamped with, or null: unknown
//   r  the target's revision at that time
//   u  the path of the page's target: "/", or its mount and a slash
//   p  the subprotocols of the framework's HMR socket
//
// When the page's own HMR socket opens, the script asks the daemon for the
// current generation and revision, until one answer succeeds, and reloads
// the page if it missed a change while it was loading. It sends nothing
// else, contacts nothing but the page's own origin, loads no code and acts
// only through location.reload(). The two forms must behave the same:
// catchup_script_test.go runs the same cases against both.
(function (stamp) {
  var NativeWebSocket = window.WebSocket;
  var nativeFetch = window.fetch;
  var element = document.currentScript;
  var started = false;
  var finished = false;
  var asking = false;
  var attempts = 0;
  var timer;
  // The element leaves the document as it runs, so the page's DOM is the
  // app's own, as a framework hydrating the whole document expects.
  if (element) {
    element.remove();
  }
  if (!NativeWebSocket || !nativeFetch) {
    return;
  }

  // forThisTarget reports a socket to the page's own origin, at a path that
  // selects the page's target: under its mount, or, for the first target,
  // under no other target's mount.
  function forThisTarget(url) {
    try {
      var address = new URL(url, document.baseURI);
      var path = address.pathname + "/";
      return address.host === location.host && path.indexOf(stamp.u) === 0 && (stamp.u !== "/" || path.indexOf("/_purlview/t/") !== 0);
    } catch (e) {
      return false;
    }
  }

  // isHotReloadSocket recognises the framework's HMR socket: one of its
  // subprotocols, for this page's target.
  function isHotReloadSocket(url, protocols) {
    var offered = [].concat(protocols === undefined ? [] : protocols);
    for (var i = 0; i < offered.length; i++) {
      if (stamp.p.indexOf(String(offered[i])) >= 0) {
        return forThisTarget(url);
      }
    }
    return false;
  }

  // The wrapper takes the native constructor's place with its name, length,
  // prototype chain, prototype object, constants (with their property
  // attributes) and a TypeError when called without new; sockets are the
  // native ones, and their constructor property names the wrapper, as it
  // named the native constructor. What differs is the wrapper's source text
  // and, in some browsers, the wording of its TypeError.
  function WebSocket(url) {
    if (!new.target) {
      throw new TypeError("Failed to construct 'WebSocket': Please use the 'new' operator.");
    }
    var socket = Reflect.construct(NativeWebSocket, arguments, new.target);
    if (!started && isHotReloadSocket(url, arguments[1])) {
      socket.addEventListener("open", begin);
    }
    return socket;
  }
  Object.setPrototypeOf(WebSocket, Object.getPrototypeOf(NativeWebSocket));
  ["prototype", "CONNECTING", "OPEN", "CLOSING", "CLOSED"].forEach(function (name) {
    Object.defineProperty(WebSocket, name, Object.getOwnPropertyDescriptor(NativeWebSocket, name));
  });
  window.WebSocket = WebSocket;
  // A page that made WebSocket read-only keeps the native constructor, and
  // its prototype keeps naming that one.
  if (window.WebSocket === WebSocket) {
    NativeWebSocket.prototype.constructor = WebSocket;
  }

  function begin() {
    if (started) {
      return;
    }
    started = true;
    document.addEventListener("visibilitychange", function () {
      if (!document.hidden) {
        ask();
      }
    });
    ask();
  }

  // ask asks the daemon once, at the page's own origin whatever the page's
  // <base>. A 404, or a 200 that is not the daemon's JSON (from a service
  // worker, say), means there is nothing to ask: stop. Any other failure
  // retries.
  function ask() {
    if (finished || asking) {
      return;
    }
    asking = true;
    clearTimeout(timer);
    nativeFetch(new URL(stamp.u, location.href).href, { headers: { "Purlview-Revision": "1" }, cache: "no-store" })
      .then(function (response) {
        if (response.status === 404 || (response.status === 200 && !/json/.test(response.headers.get("Content-Type")))) {
          return null;
        }
        if (response.status !== 200) {
          throw new Error("unavailable");
        }
        return response.json();
      })
      .then(
        function (answer) {
          finished = true;
          if (answer) {
            decide(answer);
          }
        },
        function () {
          asking = false;
          retry();
        }
      );
  }

  // retry waits 1, 2, 4 and 8 s, then 10 s at a time while the page is
  // visible; a hidden page asks again when it becomes visible.
  function retry() {
    var delay = attempts < 4 ? 1000 << attempts : 10000;
    attempts++;
    timer = setTimeout(function () {
      if (attempts <= 4 || !document.hidden) {
        ask();
      }
    }, delay);
  }

  // decide reloads once if the page missed something: a newer revision
  // always; a changed generation or an unknown stamp at most once per tab
  // every 30 s, and never where sessionStorage is unavailable.
  function decide(answer) {
    if (stamp.g !== null && answer.r > stamp.r) {
      location.reload();
    } else if ((stamp.g === null || answer.g !== stamp.g) && mayReloadAgain()) {
      location.reload();
    }
  }

  function mayReloadAgain() {
    try {
      var key = "purlview-catch-up";
      var now = String(Date.now());
      var last = Number(sessionStorage.getItem(key));
      if (last && Math.abs(Date.now() - last) < 30000) {
        return false;
      }
      sessionStorage.setItem(key, now);
      return sessionStorage.getItem(key) === now;
    } catch (e) {
      return false;
    }
  }
})
