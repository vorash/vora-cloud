// Vora Cloud dashboard: progressive enhancement only (ADR 004). Every page
// works without this file. It never writes HTML: it sets text and classes.
"use strict";
(function () {
  var MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
  var STATUSES = ["queued", "building", "pushing", "deploying", "ready", "failed"];

  // Same wording and thresholds as relTime in view.go.
  function ago(n, unit) { return n + " " + unit + (n === 1 ? "" : "s") + " ago"; }
  function relTime(now, then) {
    var s = Math.max(0, (now - then) / 1000);
    if (s < 60) return "just now";
    if (s < 3600) return ago(Math.floor(s / 60), "minute");
    if (s < 86400) return ago(Math.floor(s / 3600), "hour");
    if (s < 2592000) return ago(Math.floor(s / 86400), "day");
    var d = new Date(then);
    return d.getUTCDate() + " " + MONTHS[d.getUTCMonth()] + " " + d.getUTCFullYear();
  }
  function tickTimes() {
    var now = Date.now();
    document.querySelectorAll("time[datetime]").forEach(function (el) {
      var t = Date.parse(el.getAttribute("datetime"));
      if (!isNaN(t)) el.textContent = relTime(now, t);
    });
  }
  setInterval(tickTimes, 30000);

  // A message in the page's status region; it clears itself.
  var flashTimer;
  function flash(text) {
    var box = document.getElementById("flash");
    if (!box) return;
    var p = document.createElement("p");
    p.className = "alert alert-info";
    p.textContent = text;
    box.replaceChildren(p);
    clearTimeout(flashTimer);
    flashTimer = setTimeout(function () { box.replaceChildren(); }, 6000);
  }

  // Live status: the server pushes the newest deployments' states.
  function setError(li, text) {
    var box = li.querySelector(".tl-err"), summary = box && box.querySelector("summary");
    if (!text) { if (box) box.remove(); return; }
    if (!box) {
      var details = document.createElement("details");
      details.className = "err";
      summary = details.appendChild(document.createElement("summary"));
      box = document.createElement("div");
      box.className = "tl-err";
      box.appendChild(details);
      li.appendChild(box);
    }
    summary.textContent = text;
  }
  function applyStatus(deployments) {
    var els = document.querySelectorAll("[data-deployment]");
    deployments.forEach(function (dep) {
      if (STATUSES.indexOf(dep.status) < 0) return;
      els.forEach(function (el) {
        if (el.dataset.deployment !== dep.id) return;
        var chip = el.querySelector(".status");
        if (chip) {
          chip.className = "status status-" + dep.status;
          chip.lastChild.nodeValue = dep.status;
        }
        if (el.classList.contains("tl-item")) setError(el, dep.error);
      });
    });
  }
  var live = document.querySelector("[data-events]");
  if (live && window.EventSource) {
    var note = document.querySelector("[data-live-note]");
    var idle = note && note.textContent;
    var source = new EventSource(live.dataset.events);
    source.onopen = function () { if (note) note.textContent = "Updating live."; };
    source.addEventListener("status", function (e) { applyStatus(JSON.parse(e.data).deployments); });
    // Every deployment is final: close, or the browser would reconnect.
    source.addEventListener("done", function () { source.close(); if (note) note.textContent = idle; });
    source.onerror = function () { if (note && source.readyState === EventSource.CLOSED) note.textContent = idle; };
  }

  // Copy the project's address.
  var address = document.querySelector("a[data-copy]");
  if (address && navigator.clipboard) {
    var copy = document.createElement("button");
    copy.type = "button";
    copy.className = "btn btn-secondary btn-sm";
    copy.textContent = "Copy address";
    copy.addEventListener("click", function () {
      navigator.clipboard.writeText(address.href).then(
        function () { flash("Address copied."); },
        function () { flash("Could not copy the address."); }
      );
    });
    address.parentNode.appendChild(copy);
  }

  // Deploy and Redeploy: show they are working and refuse a second submit.
  document.querySelectorAll("form[data-busy]").forEach(function (form) {
    var button = form.querySelector("button[type=submit]"), label = button.textContent;
    form.addEventListener("submit", function (e) {
      if (form.getAttribute("aria-busy") === "true") { e.preventDefault(); return; }
      form.setAttribute("aria-busy", "true");
      button.disabled = true;
      button.textContent = form.dataset.busy;
    });
    // Coming back with the Back button can restore the page as it was left.
    window.addEventListener("pageshow", function (e) {
      if (!e.persisted) return;
      form.removeAttribute("aria-busy");
      button.disabled = false;
      button.textContent = label;
    });
  });

  // A deploy or redeploy redirects to the new deployment's row; say so.
  var nav = performance.getEntriesByType("navigation")[0];
  if (/^#deployment-/.test(location.hash) && nav && nav.type === "navigate" && document.referrer) {
    try {
      if (new URL(document.referrer).pathname === location.pathname) flash("Deployment started. Its status is below.");
    } catch (e) { /* an unusable referrer: no message */ }
  }
})();
