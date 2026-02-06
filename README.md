# HTTP Proxy Over WebRTC Datachannel

Идея: поднять локальный HTTP proxy на машине A (`go1`), а выход в интернет делать с машины B (`go2`) через WebRTC datachannel. Между браузером и Go процессами используется WebSocket.

Схема:
- Tab A: WebRTC datachannel <-> WS `127.0.0.1:9000/ws` <-> `go1` (HTTP proxy `127.0.0.1:8080`)
- Tab B: WebRTC datachannel <-> WS `127.0.0.1:9001/ws` <-> `go2` (egress TCP dial)

## Run

На сервере (машина B):
```bash
cd ~/max
go run ./go2
```

Локально (машина A):
```bash
cd /path/to/max
go run ./go1 -proxy-listen 127.0.0.1:8080
```

## Inject

Нужно 2 вкладки (A и B), в каждой открыть DevTools Console и вставить соответствующий инжект.

Важно: инжект нужно вставлять **до установления WebRTC соединения** (то есть до начала/подключения к звонку). Если вставить после, `RTCPeerConnection` и datachannel уже могут быть созданы, и хук не сработает.

### Tab A (local, WS -> go1)

![](img/inject%20here.jpg)

## Inject A (tab A → go1)
```js
(() => {
  const LABEL = "customBytes";
  const ID = 2;
  let dc = null, readyResolve;
  window.__dcReady = new Promise(res => readyResolve = res);

  const ws = new WebSocket("ws://127.0.0.1:9000/ws");
  ws.binaryType = "arraybuffer";

  function attach(ch){
    dc = ch; window.__dc = ch;
    dc.binaryType = "arraybuffer";
    dc.addEventListener("open", () => {
      console.log("[DC] open A");
      readyResolve(ch);
    });
    dc.addEventListener("message", (e) => {
      if (ws.readyState === 1) ws.send(e.data);
      window.__onDcMessage?.(e.data);
    });
  }

  const Orig = window.RTCPeerConnection;
  window.RTCPeerConnection = function (...args) {
    const pc = new Orig(...args);
    pc.addEventListener("datachannel", (ev) => {
      if (ev.channel?.label === LABEL) attach(ev.channel);
    });
    try { attach(pc.createDataChannel(LABEL, { negotiated: true, id: ID })); } catch (_) {}
    return pc;
  };
  window.RTCPeerConnection.prototype = Orig.prototype;

  ws.onmessage = (e) => dc?.readyState === "open" && dc.send(e.data);
  ws.onopen = () => console.log("[WS] open A");
  window.__dcSend = (buf) => dc?.readyState === "open" && dc.send(buf);
})();
```

### Tab B (server, WS -> go2)

![](img/inject%20server.jpg)

## Inject B (tab B → go2)
```js
(() => {
  const LABEL = "customBytes";
  const ID = 2;
  let dc = null, readyResolve;
  window.__dcReady = new Promise(res => readyResolve = res);

  const ws = new WebSocket("ws://127.0.0.1:9001/ws");
  ws.binaryType = "arraybuffer";

  function attach(ch){
    dc = ch; window.__dc = ch;
    dc.binaryType = "arraybuffer";
    dc.addEventListener("open", () => {
      console.log("[DC] open B");
      readyResolve(ch);
    });
    dc.addEventListener("message", (e) => {
      if (ws.readyState === 1) ws.send(e.data);
      window.__onDcMessage?.(e.data);
    });
  }

  const Orig = window.RTCPeerConnection;
  window.RTCPeerConnection = function (...args) {
    const pc = new Orig(...args);
    pc.addEventListener("datachannel", (ev) => {
      if (ev.channel?.label === LABEL) attach(ev.channel);
    });
    try { attach(pc.createDataChannel(LABEL, { negotiated: true, id: ID })); } catch (_) {}
    return pc;
  };
  window.RTCPeerConnection.prototype = Orig.prototype;

  ws.onmessage = (e) => dc?.readyState === "open" && dc.send(e.data);
  ws.onopen = () => console.log("[WS] open B");
  window.__dcSend = (buf) => dc?.readyState === "open" && dc.send(buf);
})();
```

## Verify (curl)

Когда оба инжекта вставлены, инициируй звонок/подключение к звонку и дождись логов в консоли вкладок: `[WS] open ...` и `[DC] open ...`.

После того как обе вкладки показали `[WS] open ...` и `[DC] open ...`, можно делать запросы через proxy на машине A:
```bash
curl -x http://127.0.0.1:8080 https://api.ipify.org
```

Ожидаемо: вернется публичный IP машины B (сервер).

![](img/proxy%20is%20working.jpg)

## Debug

`go1` и `go2` по умолчанию печатают счетчики байт по транспорту:

Локально (go1):
![](img/logs%20here.jpg)

На сервере (go2):
![](img/logs%20server.jpg)

Флаги:
- `-stats=true|false`
- `-stats-interval=5s`
