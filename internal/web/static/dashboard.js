// Shared dashboard helpers: clipboard, local timestamps, HTMX 400 flash swaps, Escape cancel, focus.

function airouterFormatLocalTimes(root) {
  const scope = root && root.querySelectorAll ? root : document;
  const times = scope.querySelectorAll("time[data-local-time]");
  const dateFormatter = new Intl.DateTimeFormat(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
  const timeFormatter = new Intl.DateTimeFormat(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    timeZoneName: "short",
  });
  const fullFormatter = new Intl.DateTimeFormat(undefined, {
    dateStyle: "full",
    timeStyle: "long",
  });

  times.forEach(function (el) {
    if (el.dataset.localTime === "formatted") return;
    const instant = new Date(el.dateTime);
    if (Number.isNaN(instant.getTime())) return;

    const date = el.querySelector(".log-time-date");
    const clock = el.querySelector(".log-time-clock");
    if (date) date.textContent = dateFormatter.format(instant);
    if (clock) clock.textContent = timeFormatter.format(instant);

    const utcTitle = el.getAttribute("title");
    el.title = fullFormatter.format(instant) + (utcTitle ? "\n" + utcTitle : "");
    el.dataset.localTime = "formatted";
  });
}

function airouterMarkCopied(btn) {
  if (!btn) return;
  const prev = btn.textContent;
  btn.textContent = "Copied";
  window.setTimeout(function () {
    btn.textContent = prev;
  }, 1500);
}

function airouterCopyFromPrevious(btn) {
  const el = btn && btn.previousElementSibling;
  if (!el) return;
  const text = el.value != null ? el.value : el.textContent;
  if (text == null) return;
  navigator.clipboard.writeText(String(text)).then(function () {
    airouterMarkCopied(btn);
  });
}

function airouterCopyText(text, btn) {
  if (text == null) return;
  navigator.clipboard.writeText(String(text)).then(function () {
    airouterMarkCopied(btn);
  });
}

// Allow 400 validation responses (styled flash HTML) to swap into flash sinks.
// Status stays non-2xx so form after-request "successful" handlers do not run.
document.body.addEventListener("htmx:beforeSwap", function (event) {
  const xhr = event.detail.xhr;
  if (xhr && xhr.status === 400) {
    event.detail.shouldSwap = true;
    event.detail.isError = false;
  }
});

function airouterFocusFirstField(root) {
  if (!root) return;
  const el = root.querySelector(
    'input:not([type="hidden"]):not([type="checkbox"]):not([disabled]), select:not([disabled]), textarea:not([disabled])'
  );
  if (el && typeof el.focus === "function") {
    el.focus({ preventScroll: true });
  }
}

function airouterIsEditRowTarget(t) {
  return !!t && (/^provider-\d+$/.test(t.id) || /^combo-\d+$/.test(t.id));
}

let airouterLogsScrollY = null;

document.body.addEventListener("htmx:beforeSwap", function (event) {
  const target = event.detail.target;
  if (target && target.id === "logs-body") {
    airouterLogsScrollY = window.scrollY;
  }
});

document.body.addEventListener("htmx:afterSwap", function (event) {
  const t = event.detail.target;
  if (!t) return;
  airouterFormatLocalTimes(document);
  if (t.id === "logs-body" && airouterLogsScrollY !== null) {
    const documentHeight = Math.max(
      document.documentElement.scrollHeight,
      document.body.scrollHeight
    );
    const maxScrollY = Math.max(0, documentHeight - window.innerHeight);
    window.scrollTo({ top: airouterLogsScrollY <= maxScrollY ? airouterLogsScrollY : 0 });
    airouterLogsScrollY = null;
  }
  if (t.id === "provider-form-slot") {
    airouterFocusFirstField(t);
    return;
  }
  if (airouterIsEditRowTarget(t) && t.querySelector("form")) {
    airouterFocusFirstField(t);
  }
});

airouterFormatLocalTimes(document);

document.addEventListener("click", function (event) {
  const toggle = event.target && event.target.closest
    ? event.target.closest("[data-log-error-toggle]")
    : null;
  if (!toggle) return;

  const detail = document.getElementById(toggle.getAttribute("aria-controls"));
  if (!detail) return;
  const expanded = toggle.getAttribute("aria-expanded") === "true";
  toggle.setAttribute("aria-expanded", String(!expanded));
  const label = toggle.getAttribute("aria-label") || "";
  toggle.setAttribute(
    "aria-label",
    label.replace(expanded ? /^Hide / : /^Show /, expanded ? "Show " : "Hide ")
  );
  detail.hidden = expanded;
});

// Escape cancels open create/edit forms without submitting.
document.addEventListener("keydown", function (event) {
  if (event.key !== "Escape") return;
  const slot = document.getElementById("provider-form-slot");
  if (slot && slot.innerHTML.trim() !== "") {
    if (typeof airouterCloseProviderForm === "function") {
      airouterCloseProviderForm();
    }
    event.preventDefault();
    return;
  }
  const form = event.target && event.target.closest
    ? event.target.closest("tr form.provider-form, tr form.combo-form")
    : null;
  if (!form) return;
  const cancel = form.querySelector(
    'button[hx-get*="/row"], button.link[hx-get*="/row"]'
  );
  if (cancel) {
    cancel.click();
    event.preventDefault();
  }
});
