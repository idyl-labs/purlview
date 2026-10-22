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

// Runs the catch-up script against a simulated page: a WebSocket the test
// opens, fetch answers the test chooses, timers the test fires, and
// sessionStorage the test controls. Usage: node catchup-harness.js <script>,
// where <script> holds the script's function expression. It prints one line
// per case and exits non-zero if any case fails.
"use strict";

const fs = require("fs");
const vm = require("vm");

const source = fs.readFileSync(process.argv[2], "utf8");
const host = "k7m2p4qx.purlview.invalid";

function memoryStorage() {
  const items = new Map();
  return {
    getItem: (k) => (items.has(k) ? items.get(k) : null),
    setItem: (k, v) => items.set(k, String(v)),
  };
}

// A sessionStorage that accepts writes and keeps none, as some browsers'
// storage does when it is full or partitioned away.
function forgetfulStorage() {
  return { getItem: () => null, setItem: () => {} };
}

// page loads the script with a stamp. storage is a sessionStorage, or null
// for one that throws on every access, as in some private or sandboxed
// contexts. base is the document's base URL, as a <base href> sets it.
// readOnly makes window.WebSocket read-only, as a page might.
function page(stamp, storage, base, readOnly) {
  const env = { reloads: 0, fetches: [], timers: [], answers: [], listeners: {}, sockets: 0, removed: 0 };
  // NativeWebSocket stands in for the browser's: a class whose prototype
  // and constants have the attributes WebIDL gives them.
  class NativeWebSocket extends EventTarget {
    constructor(url, protocols) {
      super();
      this.url = url;
      this.protocols = protocols;
      this.handlers = {};
      env.sockets++;
    }
    addEventListener(type, f) {
      (this.handlers[type] = this.handlers[type] || []).push(f);
    }
    open() {
      (this.handlers.open || []).forEach((f) => f());
    }
  }
  ["CONNECTING", "OPEN", "CLOSING", "CLOSED"].forEach((name, i) => Object.defineProperty(NativeWebSocket, name, { value: i, writable: false, enumerable: true, configurable: false }));
  env.NativeWebSocket = NativeWebSocket;
  const origin = "https://" + host;
  const g = {
    URL,
    EventTarget,
    console,
    fetch(url, init) {
      env.fetches.push({ url, init });
      const a = env.answers.shift() || { fail: true };
      if (a.fail) {
        return Promise.reject(new TypeError("network"));
      }
      if (a.pending) {
        return new Promise(() => {});
      }
      const type = a.type === undefined ? (a.status === 200 ? "application/json" : "text/plain") : a.type;
      return Promise.resolve({ status: a.status, headers: { get: (h) => (h.toLowerCase() === "content-type" ? type : null) }, json: () => Promise.resolve(a.body) });
    },
    location: { href: origin + "/page?x=1", host, reload: () => env.reloads++ },
    document: {
      hidden: false,
      baseURI: base || origin + "/page?x=1",
      currentScript: { remove: () => env.removed++ },
      addEventListener(type, f) {
        (env.listeners[type] = env.listeners[type] || []).push(f);
      },
    },
    setTimeout(f, delay) {
      env.timers.push({ f, delay, done: false });
      return env.timers.length;
    },
    clearTimeout(id) {
      if (id && env.timers[id - 1]) {
        env.timers[id - 1].done = true;
      }
    },
  };
  Object.defineProperty(g, "WebSocket", { value: NativeWebSocket, writable: !readOnly, enumerable: false, configurable: true });
  if (storage) {
    g.sessionStorage = storage;
  } else {
    Object.defineProperty(g, "sessionStorage", {
      get() {
        throw new Error("SecurityError");
      },
    });
  }
  env.ctx = vm.createContext(g);
  vm.runInContext("globalThis.window = globalThis;", env.ctx);
  vm.runInContext(source + "(" + JSON.stringify(stamp) + ")", env.ctx);
  env.run = (code) => vm.runInContext(code, env.ctx);
  // socket opens a WebSocket as the app's code would.
  env.socket = (url, protocols) => {
    env.last = env.run("new WebSocket(" + JSON.stringify(url) + (protocols === undefined ? "" : ", " + JSON.stringify(protocols)) + ")");
    return env.last;
  };
  // fire runs the next pending timer and returns its delay.
  env.fire = () => {
    const t = env.timers.find((x) => !x.done);
    if (!t) {
      return null;
    }
    t.done = true;
    t.f();
    return t.delay;
  };
  env.pendingTimers = () => env.timers.filter((x) => !x.done).length;
  env.visibility = (hidden) => {
    g.document.hidden = hidden;
    (env.listeners.visibilitychange || []).forEach((f) => f());
  };
  env.show = () => env.visibility(false);
  env.hide = () => {
    g.document.hidden = true;
  };
  return env;
}

const settle = async () => {
  for (let i = 0; i < 5; i++) {
    await new Promise((resolve) => setImmediate(resolve));
  }
};
const ok = (body) => ({ status: 200, body });
const hmr = "wss://" + host + "/?token=SyntheticTok3n";
const stamp = (g, r, u) => ({ g, r, u: u || "/", p: ["vite-hmr"] });

async function opened(env, answer) {
  if (answer) {
    env.answers.push(answer);
  }
  env.socket(hmr, "vite-hmr").open();
  await settle();
}

function expect(cond, message) {
  if (!cond) {
    throw new Error(message);
  }
}

const cases = {
  "a newer revision reloads": async () => {
    const env = page(stamp("7", 3), memoryStorage());
    expect(env.fetches.length === 0, "asked before the socket opened");
    await opened(env, ok({ g: "7", r: 4 }));
    expect(env.reloads === 1, "reloads " + env.reloads);
    const f = env.fetches[0];
    expect(env.fetches.length === 1 && f.url === "https://" + host + "/" && f.init.headers["Purlview-Revision"] === "1" && f.init.cache === "no-store", "asked " + JSON.stringify(env.fetches));
  },
  "the same revision and generation do nothing": async () => {
    const env = page(stamp("7", 3, "/_purlview/t/2/"), memoryStorage());
    env.answers.push(ok({ g: "7", r: 3 }));
    env.socket("wss://" + host + "/_purlview/t/2/?token=x", "vite-hmr").open();
    await settle();
    expect(env.reloads === 0 && env.fetches.length === 1 && env.fetches[0].url === "https://" + host + "/_purlview/t/2/", "reloaded or asked elsewhere: " + JSON.stringify(env.fetches));
    expect(env.fire() === null, "a timer after success");
  },
  "the check goes to the page's own origin whatever its <base>": async () => {
    const env = page(stamp("7", 3), memoryStorage(), "https://cdn.example.invalid/assets/");
    await opened(env, ok({ g: "7", r: 3 }));
    expect(env.fetches.length === 1 && env.fetches[0].url === "https://" + host + "/", "asked " + JSON.stringify(env.fetches));
  },
  "the script element removes itself": async () => {
    const env = page(stamp("7", 3), memoryStorage());
    expect(env.removed === 1, "removed " + env.removed);
  },
  "a changed generation reloads once per tab in 30 s": async () => {
    const storage = memoryStorage();
    const first = page(stamp("7", 3), storage);
    await opened(first, ok({ g: "8", r: 0 }));
    expect(first.reloads === 1, "first page " + first.reloads);
    expect(/^\d+$/.test(storage.getItem("purlview-catch-up")), "recorded " + storage.getItem("purlview-catch-up"));
    const second = page(stamp("8", 0), storage);
    await opened(second, ok({ g: "9", r: 0 }));
    expect(second.reloads === 0, "reloaded twice within 30 s");
    storage.setItem("purlview-catch-up", String(Date.now() - 31000));
    const third = page(stamp("9", 0), storage);
    await opened(third, ok({ g: "10", r: 0 }));
    expect(third.reloads === 1, "no reload after 30 s");
  },
  "an unknown stamp reloads once per tab in 30 s": async () => {
    const storage = memoryStorage();
    const first = page(stamp(null, 0), storage);
    await opened(first, ok({ g: "7", r: 0 }));
    const second = page(stamp(null, 0), storage);
    await opened(second, ok({ g: "7", r: 5 }));
    expect(first.reloads === 1 && second.reloads === 0, "reloads " + first.reloads + " " + second.reloads);
  },
  "a newer revision reloads within the 30 s too": async () => {
    const storage = memoryStorage();
    storage.setItem("purlview-catch-up", String(Date.now()));
    const env = page(stamp("7", 3), storage);
    await opened(env, ok({ g: "8", r: 4 }));
    expect(env.reloads === 1, "reloads " + env.reloads);
  },
  "without sessionStorage only a newer revision reloads": async () => {
    for (const storage of [null, forgetfulStorage()]) {
      for (const [s, answer, want] of [
        [stamp("7", 3), { g: "8", r: 3 }, 0],
        [stamp(null, 0), { g: "8", r: 9 }, 0],
        [stamp("7", 3), { g: "7", r: 4 }, 1],
      ]) {
        const env = page(s, storage);
        await opened(env, ok(answer));
        expect(env.reloads === want, (storage ? "forgetful " : "throwing ") + JSON.stringify(s) + " " + JSON.stringify(answer) + ": reloads " + env.reloads);
      }
    }
  },
  "failures retry after 1, 2, 4 and 8 s, then every 10 s, and reload only after success": async () => {
    const env = page(stamp("7", 3), memoryStorage());
    env.answers.push({ status: 503 }, { fail: true }, { status: 502 }, { status: 503 }, { status: 503 }, { status: 503 });
    await opened(env);
    const delays = [];
    for (let i = 0; i < 6; i++) {
      expect(env.reloads === 0, "reloaded before a successful answer");
      expect(env.pendingTimers() === 1, "timers pending: " + env.pendingTimers());
      delays.push(env.fire());
      await settle();
    }
    env.answers.push(ok({ g: "7", r: 4 }));
    delays.push(env.fire());
    await settle();
    expect(JSON.stringify(delays) === "[1000,2000,4000,8000,10000,10000,10000]", "delays " + JSON.stringify(delays));
    expect(env.fetches.length === 8 && env.reloads === 1, "fetches " + env.fetches.length + " reloads " + env.reloads);
  },
  "a hidden page retries 4 times, then waits until it is visible": async () => {
    const env = page(stamp("7", 3), memoryStorage());
    env.hide();
    await opened(env, { status: 503 });
    for (let i = 0; i < 4; i++) {
      env.answers.push({ status: 503 });
      env.fire();
      await settle();
    }
    expect(env.fetches.length === 5, "first retries while hidden: " + env.fetches.length);
    env.fire(); // 10 s later, still hidden
    await settle();
    expect(env.fetches.length === 5, "asked while hidden");
    env.visibility(true); // a change that leaves the page hidden
    await settle();
    expect(env.fetches.length === 5, "asked on becoming hidden");
    env.answers.push(ok({ g: "7", r: 4 }));
    env.show();
    await settle();
    expect(env.fetches.length === 6 && env.reloads === 1, "on becoming visible: " + env.fetches.length + " " + env.reloads);
    env.show();
    await settle();
    expect(env.fetches.length === 6, "asked again after success");
  },
  "becoming visible during a pending retry asks once and replaces the retry": async () => {
    const env = page(stamp("7", 3), memoryStorage());
    await opened(env, { status: 503 });
    expect(env.pendingTimers() === 1, "no retry pending");
    env.answers.push({ status: 503 });
    env.show();
    await settle();
    expect(env.fetches.length === 2 && env.pendingTimers() === 1, "fetches " + env.fetches.length + ", timers pending " + env.pendingTimers());
  },
  "a question in flight is not asked again": async () => {
    const env = page(stamp("7", 3), memoryStorage());
    await opened(env, { pending: true });
    env.show();
    env.show();
    await settle();
    expect(env.fetches.length === 1, "asked " + env.fetches.length + " times at once");
  },
  "a 404 ends the checks": async () => {
    const env = page(stamp("7", 3), memoryStorage());
    await opened(env, { status: 404 });
    env.show();
    await settle();
    expect(env.fetches.length === 1 && env.reloads === 0 && env.fire() === null, "after 404: " + env.fetches.length);
  },
  "a 200 that is not JSON ends the checks": async () => {
    for (const type of ["text/html; charset=utf-8", null]) {
      const env = page(stamp(null, 0), memoryStorage());
      await opened(env, { status: 200, type, body: { g: "7", r: 0 } });
      env.show();
      await settle();
      expect(env.fetches.length === 1 && env.reloads === 0 && env.fire() === null, type + ": " + env.fetches.length + " " + env.reloads);
    }
  },
  "only the framework's socket for this page's target starts the check": async () => {
    const env = page(stamp("7", 3), memoryStorage());
    env.socket(hmr, "vite-ping").open();
    env.socket("ws://localhost:5173/?token=x", "vite-hmr").open();
    env.socket(hmr).open();
    env.socket("/relative", ["graphql-ws"]).open();
    env.socket("wss://" + host + "/_purlview/t/2/?token=x", "vite-hmr").open();
    await settle();
    expect(env.fetches.length === 0, "asked for another socket");
    env.answers.push(ok({ g: "7", r: 3 }));
    env.socket("/", ["x", "vite-hmr"]).open();
    await settle();
    expect(env.fetches.length === 1, "no check for the HMR socket with a relative URL");
    // A later HMR socket, as after a reconnect, does not ask again.
    env.socket(hmr, "vite-hmr").open();
    await settle();
    expect(env.fetches.length === 1, "asked twice");
    // A mounted target's page: its own mount only.
    const mounted = page(stamp("7", 3, "/_purlview/t/2/"), memoryStorage());
    mounted.socket(hmr, "vite-hmr").open();
    mounted.socket("wss://" + host + "/_purlview/t/23/", "vite-hmr").open();
    await settle();
    expect(mounted.fetches.length === 0, "a mounted page asked for another target's socket");
    mounted.answers.push(ok({ g: "7", r: 3 }));
    mounted.socket("wss://" + host + "/_purlview/t/2", "vite-hmr").open();
    await settle();
    expect(mounted.fetches.length === 1, "a mounted page did not ask for its own socket");
  },
  "a relative HMR address is resolved against the page's <base>": async () => {
    const mounted = page(stamp("7", 3, "/_purlview/t/2/"), memoryStorage(), "https://" + host + "/_purlview/t/2/");
    mounted.answers.push(ok({ g: "7", r: 3 }));
    mounted.socket("./?token=x", "vite-hmr").open();
    await settle();
    expect(mounted.fetches.length === 1, "a mounted page did not ask for its socket under <base>");
    const root = page(stamp("7", 3), memoryStorage(), "https://" + host + "/_purlview/t/2/");
    root.socket("./?token=x", "vite-hmr").open();
    await settle();
    expect(root.fetches.length === 0, "the first target's page asked for a socket that resolves under another mount");
  },
  "a read-only WebSocket is left alone": async () => {
    const env = page(stamp("7", 3), memoryStorage(), undefined, true);
    const s = env.socket(hmr, "vite-hmr");
    expect(env.run("WebSocket") === env.NativeWebSocket && s.constructor === env.NativeWebSocket && env.NativeWebSocket.prototype.constructor === env.NativeWebSocket, "the native constructor was changed");
    s.open();
    await settle();
    expect(env.fetches.length === 0 && env.reloads === 0, "acted without its wrapper installed");
  },
  "two HMR sockets made before either opens ask once": async () => {
    const env = page(stamp("7", 3), memoryStorage());
    const a = env.socket(hmr, "vite-hmr");
    const b = env.socket(hmr, "vite-hmr");
    env.answers.push({ status: 503 });
    a.open();
    b.open();
    await settle();
    expect(env.fetches.length === 1 && (env.listeners.visibilitychange || []).length === 1, "fetches " + env.fetches.length + ", listeners " + (env.listeners.visibilitychange || []).length);
  },
  "WebSocket keeps the native constructor's shape": async () => {
    const env = page(stamp("7", 3), memoryStorage());
    const r = env.run(`(() => {
      const s = new WebSocket("wss://${host}/x");
      class Sub extends WebSocket { hello() { return "hi"; } }
      const sub = new Sub("wss://${host}/y", "vite-hmr");
      const d = (k) => JSON.stringify(Object.getOwnPropertyDescriptor(WebSocket, k));
      let called = "no error";
      try { WebSocket("wss://${host}/z"); } catch (e) { called = e instanceof TypeError && e.message.includes("Please use the 'new' operator") ? "TypeError" : String(e); }
      return [s instanceof WebSocket, Object.getPrototypeOf(s) === WebSocket.prototype, s.constructor === WebSocket, WebSocket.name, WebSocket.length,
        Object.getPrototypeOf(WebSocket) === EventTarget, d("prototype"), d("OPEN"), WebSocket.CONNECTING, WebSocket.CLOSING, WebSocket.CLOSED,
        sub.hello(), sub instanceof Sub, sub instanceof WebSocket, s.url, sub.protocols, called].join(" | ");
    })()`);
    const want = [true, true, true, "WebSocket", 1, true, '{"writable":false,"enumerable":false,"configurable":false}', '{"value":1,"writable":false,"enumerable":true,"configurable":false}', 0, 2, 3, "hi", true, true, "wss://" + host + "/x", "vite-hmr", "TypeError"].join(" | ");
    const got = r.replace(/"value":\{\},/, "");
    expect(got === want, "\n got " + r + "\nwant " + want);
    expect(env.run("WebSocket.prototype") === env.NativeWebSocket.prototype && env.sockets === 2, "not the native prototype");
  },
  "without WebSocket or fetch the script does nothing": async () => {
    let removed = 0;
    const g = { location: { reload() { throw new Error("reload"); } }, document: { currentScript: { remove: () => removed++ } } };
    const ctx = vm.createContext(g);
    vm.runInContext("globalThis.window = globalThis;", ctx);
    vm.runInContext(source + '({"g":"7","r":3,"u":"/","p":["vite-hmr"]})', ctx);
    expect(g.WebSocket === undefined && removed === 1, "defined WebSocket, or left its element");
  },
};

(async () => {
  let failed = 0;
  for (const [name, run] of Object.entries(cases)) {
    try {
      await run();
      console.log("ok   " + name);
    } catch (e) {
      failed++;
      console.log("FAIL " + name + ": " + e.message);
    }
  }
  process.exit(failed ? 1 : 0);
})();
