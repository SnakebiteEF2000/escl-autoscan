// Keeps the page current without reloading it: every two seconds it fetches
// the page again and replaces the parts marked data-live that have changed.
// The server renders everything; there is no second copy of the markup here.
// Without JavaScript the page reloads itself instead (see <noscript>).
(function () {
  "use strict";

  var INTERVAL = 2000;
  var timer = null;
  var stale = document.getElementById("stale");

  // A rejected POST is answered at /scan, /pause, ...; make "reload" a GET of /.
  if (location.pathname !== "/") {
    history.replaceState(null, "", "/");
  }

  function apply(html) {
    var fresh = new DOMParser().parseFromString(html, "text/html");
    document.querySelectorAll("[data-live]").forEach(function (old) {
      var now = fresh.querySelector('[data-live="' + old.dataset.live + '"]');
      if (!now) {
        return;
      }
      if (old.className !== now.className) {
        old.className = now.className;
      }
      if (old.innerHTML !== now.innerHTML) {
        old.innerHTML = now.innerHTML;
      }
    });
  }

  function schedule() {
    clearTimeout(timer);
    if (!document.hidden) {
      timer = setTimeout(refresh, INTERVAL);
    }
  }

  function refresh() {
    fetch("/", { credentials: "same-origin", redirect: "manual", cache: "no-store" })
      .then(function (resp) {
        if (resp.type === "opaqueredirect") {
          // The session has expired: let the browser go through the login.
          location.reload();
          return null;
        }
        if (!resp.ok) {
          throw new Error("HTTP " + resp.status);
        }
        return resp.text();
      })
      .then(function (html) {
        if (html !== null) {
          apply(html);
          stale.hidden = true;
        }
      })
      .catch(function () {
        stale.hidden = false;
      })
      .then(schedule);
  }

  document.addEventListener("visibilitychange", function () {
    if (document.hidden) {
      clearTimeout(timer);
    } else {
      refresh();
    }
  });
  schedule();
})();
