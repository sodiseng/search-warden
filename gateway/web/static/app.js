// Behaviours of the admin panel. Self-hosted (CSP: script-src 'self'), no inline
// handlers; everything is delegated on document so it survives htmx swaps.
(function () {
  "use strict";

  var root = document.documentElement;
  function attr(name) { return root.getAttribute(name) || ""; }
  function $(id) { return document.getElementById(id); }

  // ---- display preferences: a cookie the server reads when it renders ----
  function setPref(name, value) {
    document.cookie = name + "=" + value + "; path=" + (attr("data-base") || "/") + "; max-age=31536000; samesite=lax" +
      (location.protocol === "https:" ? "; secure" : "");
  }

  // ---- modal: one dialog, filled from the server ----
  // <button data-modal="keys/abc"> → GET that URL as a fragment and show it
  // <button data-close>            → close it; Esc and a click on the backdrop do too
  function dlg() { return $("modal"); }
  function body() { return $("modal-body"); }

  function show(d) {
    if (!d.open) d.showModal();
    // The open state hides the page scrollbar; Pico pads the page by this much so
    // nothing jumps sideways (a CSSOM write, which the CSP allows).
    var bar = window.innerWidth - root.clientWidth;
    if (bar > 0 && !root.classList.contains("modal-is-open")) root.style.setProperty("--pico-scrollbar-width", bar + "px");
    root.classList.add("modal-is-open");
  }

  function message(cls, text) {
    var b = body();
    b.textContent = "";
    var box = document.createElement("div");
    box.className = cls;
    box.textContent = text;
    if (cls === "loading") box.setAttribute("aria-busy", "true");
    var main = document.createElement("div");
    main.className = "modal-main";
    main.appendChild(box);
    b.appendChild(main);
  }

  function open(url) {
    var d = dlg(), b = body();
    if (!d || !b) { location.href = url; return; }
    message("loading", attr("data-t-loading"));
    show(d);
    fetch(url, { headers: { "HX-Request": "true" }, credentials: "same-origin" })
      .then(function (r) {
        var login = r.headers.get("HX-Redirect");
        if (r.status === 401 && login) { location.href = login; return null; }
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.text();
      })
      .then(function (html) {
        if (html === null) return;
        b.innerHTML = html;
        if (window.htmx) window.htmx.process(b); // the fragment's hx-post forms
      })
      .catch(function (err) { message("alert err", attr("data-t-failed") + err.message); });
  }

  function close() {
    var d = dlg();
    if (d && d.open) d.close();
  }

  document.addEventListener("close", function (e) {
    if (e.target !== dlg()) return;
    root.classList.remove("modal-is-open");
    root.style.removeProperty("--pico-scrollbar-width");
    // A fragment that changed something the list shows (a new token) asks for a reload.
    var again = body().querySelector("[data-reload]");
    body().textContent = "";
    if (again) location.href = again.getAttribute("data-reload");
  }, true);

  document.addEventListener("click", function (e) {
    var t = e.target;
    if (!t.closest) return;

    var opener = t.closest("[data-modal]");
    if (opener) {
      e.preventDefault();
      open(opener.getAttribute("data-modal"));
      return;
    }
    if (t.closest("[data-close]")) {
      e.preventDefault();
      if (t.closest("dialog")) { close(); return; }
      var back = document.querySelector("[data-back]"); // the full-page fallback of a dialog
      if (back) location.href = back.href;
      return;
    }
    if (t === dlg()) { close(); return; } // the backdrop

    var theme = t.closest("[data-toggle-theme]");
    if (theme) {
      var next = attr("data-theme") === "dark" ? "light" : "dark";
      root.setAttribute("data-theme", next);
      setPref("warden_theme", next);
      return;
    }
    var lang = t.closest("[data-set-lang]");
    if (lang) {
      setPref("warden_lang", lang.getAttribute("data-set-lang"));
      // A page that answered a POST names a GET address to come back to.
      if (attr("data-self")) location.href = attr("data-self"); else location.reload();
      return;
    }
    if (t.closest("[data-toggle-nav]")) {
      document.body.classList.toggle("nav-open");
      return;
    }
    var toast = t.closest("[data-toast-close]");
    if (toast) {
      toast.closest("[data-toast]").remove();
      return;
    }

    // <button data-copy-target>: copies the [data-copy] text next to it.
    var copy = t.closest("[data-copy-target]");
    if (copy) {
      var src = copy.parentElement && copy.parentElement.querySelector("[data-copy]");
      if (!src) return;
      var done = function () { copy.lastChild.textContent = attr("data-t-copied"); };
      if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(src.textContent).then(done, function () { select(src); });
      } else {
        select(src); // plain HTTP: select it so Ctrl+C works
      }
    }
  });

  function select(el) {
    var r = document.createRange();
    r.selectNodeContents(el);
    var s = window.getSelection();
    s.removeAllRanges();
    s.addRange(r);
  }

  // A dialog form that succeeded navigates (HX-Redirect); one that failed re-renders
  // the dialog body (keep it shown: a swap can follow a close).
  document.addEventListener("htmx:afterSwap", function (e) {
    var d = dlg();
    if (d && e.target && e.target.id === "modal-body") show(d);
  });

  // ---- <button data-confirm="…">: ask in a dialog before submitting. Capture
  // phase, so declining also stops htmx from sending the request.
  var confirmed = null;
  document.addEventListener("submit", function (e) {
    var b = e.submitter;
    var msg = b && b.getAttribute("data-confirm");
    if (!msg) return;
    if (confirmed === b) { confirmed = null; return; }
    e.preventDefault();
    e.stopImmediatePropagation();
    var form = e.target, d = $("confirm");
    if (!d || !form.requestSubmit) {
      if (window.confirm(msg)) { confirmed = b; form.requestSubmit ? form.requestSubmit(b) : form.submit(); }
      return;
    }
    $("confirm-text").textContent = msg;
    var yes = $("confirm-yes");
    yes.classList.toggle("danger", b.classList.contains("danger"));
    yes.onclick = function () { d.close(); confirmed = b; form.requestSubmit(b); };
    $("confirm-no").onclick = function () { d.close(); };
    d.showModal();
  }, true);

  // ---- selection (keys): "select all" boxes and the count in the action bar ----
  function countSelected() {
    var n = document.querySelectorAll("td.pick input:checked").length;
    var out = document.querySelector("[data-batch-count]");
    if (out) out.textContent = n ? " · " + n : "";
    document.querySelectorAll("input[data-check-all]").forEach(function (all) {
      var boxes = document.querySelectorAll('input[type=checkbox][data-group="' + all.getAttribute("data-check-all") + '"]');
      var on = 0;
      boxes.forEach(function (c) { if (c.checked) on++; });
      all.checked = boxes.length > 0 && on === boxes.length;
      all.indeterminate = on > 0 && on < boxes.length;
    });
  }
  document.addEventListener("change", function (e) {
    var t = e.target;
    if (!t || !t.getAttribute) return;
    var group = t.getAttribute("data-check-all");
    if (group) {
      document.querySelectorAll('input[type=checkbox][data-group="' + group + '"]').forEach(function (c) { c.checked = t.checked; });
    }
    if (group || t.closest("td.pick")) countSelected();
  });

  // ---- on load ----
  // A confirmation came with ?ok=…: drop it from the address so a reload or the
  // language switch does not show the same toast again.
  if (document.querySelector("[data-toast]") && window.history.replaceState) {
    var u = new URL(location.href);
    if (u.searchParams.has("ok")) {
      u.searchParams.delete("ok");
      window.history.replaceState(null, "", u.pathname + u.search + u.hash);
    }
  }
  countSelected();
})();
