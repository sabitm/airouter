// Shared dashboard helpers: clipboard copy with brief "Copied" feedback.

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
