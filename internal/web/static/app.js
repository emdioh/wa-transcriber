"use strict";
(function () {
  // WhatsApp page: follow QR rotation, pairing and connection changes.
  var p = document.getElementById("pairing");
  if (p) {
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
  }

  // Logs page: start at the bottom, optional auto-refresh by polling the
  // server (a full reload is invisible when nothing changed).
  var log = document.getElementById("log");
  if (log) {
    log.scrollTop = log.scrollHeight;
    var box = document.getElementById("autorefresh");
    var stamp = document.getElementById("logstamp");
    var count = document.getElementById("logcount");
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
  }

  // Restart page: wait for the device to come back.
  var rs = document.getElementById("restart");
  if (rs) {
    var next = rs.dataset.next || "/";
    setTimeout(function poll() {
      fetch("/login", { cache: "no-store", redirect: "manual" })
        .then(function () { location.href = next; })
        .catch(function () { setTimeout(poll, 2000); });
    }, 3000);
  }
})();
