// Web Push（FCM）バックグラウンド通知の Service Worker。
// タブが閉じている/フォアグラウンドでない時にこの SW が push イベントを
// 受け、OS 通知を出す（term.js の Firestore push=再接続トリガーとは別物）。
// config は静的に埋め込めない（このプロジェクトはビルドステップ無し・
// gstatic CDN 直 import 方式）ので、SW 自身が /api/fbtoken を fetch して
// 取得する（同一オリジン fetch は cookie が乗る＝owner ログイン済み前提。
// 失敗時は何もしない＝push 無しで従来どおり）。

importScripts("https://www.gstatic.com/firebasejs/11.6.1/firebase-app-compat.js");
importScripts("https://www.gstatic.com/firebasejs/11.6.1/firebase-messaging-compat.js");

const messagingReady = (async () => {
  try {
    const r = await fetch("/api/fbtoken", { headers: { Accept: "application/json" } });
    if (!r.ok) return null; // 未設定/未認証 → push なし
    const { config } = await r.json();
    firebase.initializeApp(config);
    return firebase.messaging();
  } catch (e) {
    return null;
  }
})();

messagingReady.then((messaging) => {
  if (!messaging) return;
  messaging.onBackgroundMessage((payload) => {
    const n = payload.notification || {};
    // data.tag は「どの PC のどのセッションか」の識別子（notify.go が
    // pcName+pane key で発行）。同一セッションの連続通知は最新1件に集約、
    // 別セッションは別々に積む＝通知欄で「どのタスクが終わったか」が潰れない。
    const tag = (payload.data && payload.data.tag) || "herdr-drover-task";
    self.registration.showNotification(n.title || "herdr-drover", {
      body: n.body || "",
      tag,
    });
  });
});

// 通知クリックでコンソール（devices ページ）を開く/フォーカス。
self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  event.waitUntil(
    self.clients.matchAll({ type: "window" }).then((list) => {
      for (const c of list) if ("focus" in c) return c.focus();
      if (self.clients.openWindow) return self.clients.openWindow("/");
    })
  );
});
