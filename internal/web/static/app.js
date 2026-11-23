"use strict";
(function () {
  // localPath keeps a redirect on this device: only a plain absolute path is
  // used, never another host ("//evil.example"), a scheme ("javascript:") or a
  // backslash trick. The template writes data-next itself, but the page must
  // not become a redirector if that ever changes.
  var localPath = function (v) {
    if (typeof v !== "string" || v.charAt(0) !== "/") return "/";
    if (v.charAt(1) === "/" || v.includes("\\")) return "/";
    return v;
  };

  // go navigates once the device answers again. replace() is deliberate: the
  // "Restarting…" page must not stay in the history, or Back lands on a dead
  // connection.
  var go = function (target) {
    location.replace(localPath(target));
  };

  // WhatsApp page: follow QR rotation, pairing and connection changes.
  var initPairing = function () {
    var p = document.querySelector("#pairing");
    if (!p) return;
    setInterval(function () {
      fetch("/api/status", { cache: "no-store" })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (s) {
          if (s && (String(s.paired) !== p.dataset.paired || s.pairing !== p.dataset.pairing)) {
            location.reload();
          }
        })
        .catch(function () {});
    }, 3000);
  };

  // Logs page: start at the bottom, optional auto-refresh by polling the
  // server (a full reload is invisible when nothing changed).
  var initLogs = function () {
    var log = document.querySelector("#log");
    if (!log) return;
    log.scrollTop = log.scrollHeight;
    var box = document.querySelector("#autorefresh");
    var stamp = document.querySelector("#logstamp");
    var count = document.querySelector("#logcount");
    var key = "wat-autorefresh";
    var timer = null;
    var stampText = function () {
      var d = new Date();
      var p = function (n) { return (n < 10 ? "0" : "") + n; };
      return "updated " + p(d.getHours()) + ":" + p(d.getMinutes()) + ":" + p(d.getSeconds());
    };
    var refresh = function () {
      var level = log.dataset.level || "debug";
      fetch("/logs/live?level=" + encodeURIComponent(level), { cache: "no-store" })
        .then(function (r) { return r.ok ? r.text() : null; })
        .then(function (text) {
          if (text === null) return;
          // Only follow the tail if the reader was already at the bottom.
          var atBottom = log.scrollTop + log.clientHeight >= log.scrollHeight - 20;
          log.textContent = text;
          if (atBottom) log.scrollTop = log.scrollHeight;
          if (stamp) stamp.textContent = stampText();
          if (count) count.textContent = String(text.split("\n").filter(function (l) { return l !== ""; }).length);
        })
        .catch(function () {}); // network hiccup: keep polling
    };
    var apply = function () {
      try { localStorage.setItem(key, box.checked ? "1" : "0"); } catch (e) {}
      clearInterval(timer);
      timer = null;
      if (box.checked) {
        refresh();
        timer = setInterval(refresh, 5000);
      }
    };
    // The choice survives reloads; on load only start or stop the timer, so a
    // browser with no storage (private mode) keeps working.
    try { box.checked = localStorage.getItem(key) === "1"; } catch (e) {}
    box.addEventListener("change", apply);
    if (box.checked) {
      refresh();
      timer = setInterval(refresh, 5000);
    }
  };

  // Restart page: wait for the device to come back.
  var initRestart = function () {
    var rs = document.querySelector("#restart");
    if (!rs) return;
    var next = rs.dataset.next;
    setTimeout(function poll() {
      fetch("/login", { cache: "no-store", redirect: "manual" })
        .then(function () { go(next); })
        .catch(function () { setTimeout(poll, 2000); });
    }, 3000);
  };

  // System page: while an install to the internal storage runs, reload the
  // page so the progress and the final result show up without a click. The
  // install itself is not tied to this page (the server detaches it from the
  // request), so a reload here only refreshes what is displayed.
  var initFlash = function () {
    var card = document.querySelector('[data-flash="busy"]');
    if (!card) return;
    setTimeout(function () { location.reload(); }, 5000);
  };

  initPairing();
  initLogs();
  initRestart();
  initFlash();
})();
