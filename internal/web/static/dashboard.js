// Shared dashboard helpers: clipboard, HTMX 400 flash swaps, Escape cancel, focus.

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

document.body.addEventListener("htmx:afterSwap", function (event) {
  const t = event.detail.target;
  if (!t) return;
  if (t.id === "provider-form-slot") {
    airouterFocusFirstField(t);
    return;
  }
  if (airouterIsEditRowTarget(t) && t.querySelector("form")) {
    airouterFocusFirstField(t);
  }
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
