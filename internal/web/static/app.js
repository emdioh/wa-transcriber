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

  // Logs page: start at the bottom, optional auto-refresh.
  var log = document.getElementById("log");
  if (log) {
    log.scrollTop = log.scrollHeight;
    var box = document.getElementById("autorefresh");
    var key = "wat-autorefresh";
    try { box.checked = localStorage.getItem(key) === "1"; } catch (e) {}
    var timer = null;
    var apply = function () {
      try { localStorage.setItem(key, box.checked ? "1" : "0"); } catch (e) {}
      clearInterval(timer);
      if (box.checked) timer = setInterval(function () { location.reload(); }, 5000);
    };
    box.addEventListener("change", apply);
    apply();
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
