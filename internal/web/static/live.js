// Keeps the page current without reloading it: every two seconds it fetches
// the page again and replaces the parts marked data-live that have changed.
// The server renders everything; there is no second copy of the markup here.
// Without JavaScript the page reloads itself instead (see <noscript>).
(function () {
  "use strict";

  var INTERVAL = 2000;
  var TIMEOUT = 8000;
  var timer = null;
  var stale = document.getElementById("stale");

  // A rejected POST is answered at /scan, /pause, ...; make "reload" a GET of /.
  if (location.pathname !== "/") {
    history.replaceState(null, "", "/");
  }
  // Its message is about one click. The page is not reloaded any more, so
  // take the message away after a while.
  var rejected = document.querySelector("[role=alert]");
  if (rejected) {
    setTimeout(function () { rejected.remove(); }, 12000);
  }

  function apply(html) {
    var fresh = new DOMParser().parseFromString(html, "text/html");
    if (!fresh.querySelector("[data-live]")) {
      // Not our page: an error page or the login page of a proxy.
      throw new Error("unexpected page");
    }
    // The forms outside the live parts need the current token too: it
    // changes with the session, and without login when the service restarts.
    var token = fresh.querySelector('input[name="csrf"]');
    if (token) {
      document.querySelectorAll('input[name="csrf"]').forEach(function (input) {
        if (input.value !== token.value) {
          input.value = token.value;
        }
      });
    }
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

  function show(el, visible) {
    if (el.hidden === visible) {
      el.hidden = !visible;
    }
  }

  function schedule() {
    clearTimeout(timer);
    if (!document.hidden) {
      timer = setTimeout(refresh, INTERVAL);
    }
  }

  function refresh() {
    // A request that hangs must not stop the updates for good.
    var abort = new AbortController();
    var giveUp = setTimeout(function () { abort.abort(); }, TIMEOUT);
    fetch("/", { credentials: "same-origin", redirect: "manual", cache: "no-store", signal: abort.signal })
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
          show(stale, false);
        }
      })
      .catch(function () {
        show(stale, true);
      })
      .then(function () {
        clearTimeout(giveUp);
        schedule();
      });
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
