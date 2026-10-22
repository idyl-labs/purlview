# What visitors receive through a share

A share forwards each visitor's requests to the app on your machine and
passes its answers back. On the way, the daemon makes the changes described
here. Nothing in your project is changed: not your source, not your
configuration, and not what your own browser tabs on `localhost` receive.

## Address translation

Your app knows itself as `http://localhost:5173`; visitors know it by the
share's address.

- **Headers, always.** A `Location` or `Access-Control-Allow-Origin` that
  names a shared app's local address names the share's address instead, and
  a cookie set by an app is kept on the share's own host, without a
  `Domain`.
- **Bodies, unless you share with `--no-rewrite`.** In text bodies (HTML,
  CSS, JavaScript, JSON and the like), exact references to a shared app's
  local address become the share's address, and a further app's address
  becomes the share's address under that app's mount.

Everything else, including addresses of apps you did not share, passes
unchanged.

## Catching up with hot reload

Visitors see your changes through your app's own hot reload, as you do. A
page that reloads, though, hears about later changes only once its new hot
reload connection (its HMR WebSocket) is open, and the dev server never
resends a message that a connection missed. Locally that gap lasts
milliseconds; through a share it lasts as long as the page takes to load
again over the network. A change you save during that gap would otherwise
reach that visitor only with your next save.

So, for apps on a supported dev server, every page a visitor opens runs a
small script from Purlview. When the page's own HMR connection opens, the
script asks the share whether the app has changed since the page was
served, and if it has, reloads the page once. `--no-rewrite` turns this off
too.

**Supported:** apps served by Vite's dev server, including Astro, SvelteKit,
Nuxt and React Router in development. Other frameworks behave as they always
have. A framework is added only once its dev server is known to send each
hot reload message to every connected page in order, and to answer a
WebSocket ping in order with the messages before it; the catch-up relies on
both.

### What the script does, and does not do

- It is under 2 KB of inline JavaScript, marked
  `data-purlview="catch-up"`. It goes into the page's head before any of the
  page's own scripts that could run before it: right after the page's
  `<meta charset>` when no such script comes first. It removes its element
  from the page as it runs.
- It wraps `WebSocket` to see the page's HMR connection open, recognised by
  its subprotocol (`vite-hmr`) and its address: the page's own, under the
  path of the page's app. Every other socket is left alone. The wrapper has
  the native constructor's name, length, prototype, constants and
  prototype chain, and sockets are the browser's own; what differs is the
  wrapper's source text and, in some browsers, the wording of its
  `TypeError`.
- Once that connection opens, it sends one request to the share's own
  address, whatever the page's `<base>`, with a `Purlview-Revision` header.
  The daemon answers it; a request with that header never reaches your app.
  If the answer is not available yet, it asks again after 1, 2, 4 and 8
  seconds, then every 10 seconds while the page is visible, and when the
  page becomes visible again. It stops after the first answer.
- It reloads the page with `location.reload()`, and only:
  - when the app has sent a change since the page was served; or
  - when the daemon cannot tell (its connection to the app was interrupted,
    or not yet open when the page was served). Such a reload happens at
    most once every 30 seconds per tab, recorded in `sessionStorage` under
    the key `purlview-catch-up`, and never where `sessionStorage` is
    unavailable.
- It sends nothing about the visitor, contacts nothing but the share's own
  address and loads no other code.

### What the daemon does

- From a visitor's first page view or HMR connection until a minute after
  the last visitor's HMR connection closes, the daemon keeps one HMR
  connection of its own to your app, as a local client, and counts the
  changes the app announces. Your app sees one more hot reload client while
  visitors are present.
- It finds that connection's address in Vite's client module
  (`/@vite/client`). Where that is not where the page connects, as with a
  Vite `base` or Nuxt, it uses the address of the first visitor HMR
  connection instead. Until it knows, a page that loads Vite's client gets
  a stamp that says "unknown", so the first page of such an app may reload
  once.
- Each page is stamped with that count as it was when the page was
  requested, and the daemon answers the script's question after a WebSocket
  ping to your app comes back, so every change sent before the question has
  been counted.
- Visitor HMR connections are forwarded without compression, so the daemon
  can read their messages. Vite keeps its last error for the next page to
  connect only while no page is connected. Because the daemon's connection
  counts as one, the daemon sends the current error itself to each new
  visitor HMR connection, so a visitor who opens a page while a module fails
  to compile still sees Vite's error overlay. Your own `localhost` tabs do
  not get this: one that connects while only the daemon is connected, in
  the minute after the last visitor leaves, may not see an error overlay
  Vite would otherwise have shown it.
- To keep a page's stamp current, a stamped page is fetched from your app in
  full, and loses its `ETag` and `Last-Modified`. Its `Content-Type` also
  states the charset the page declares in a `<meta>`, if it named none: the
  script can move that declaration beyond the first 1024 bytes, where
  browsers look for it.

### When there is no script

- `purlview share --no-rewrite`;
- pages whose `Content-Security-Policy` header forbids inline scripts, or
  forbids the page's requests to its own address (`connect-src`), and pages
  with a `<meta http-equiv="Content-Security-Policy">` in their head;
- responses that are not an HTML page opened by the visitor (scripts,
  styles, data, requests made by the page itself);
- apps whose dev server is not supported.
