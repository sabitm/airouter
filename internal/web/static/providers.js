// Provider form: toggle the api-key vs oauth field groups by the selected
// auth method, and prefill the oauth config from a chosen preset's data-*
// attributes. Visibility is driven by the form's data-method attribute so a
// server-rendered edit row starts in the correct state without JS running.

function airouterToggleAuthMethod(sel) {
  const form = sel.closest(".provider-form");
  if (form) {
    form.setAttribute("data-method", sel.value);
  }
}

// airouterToggleOAuthMode drives the form's data-oauth-mode attribute so an
// interactive-oauth form shows either the web-auth (Connect) group or the manual
// token-import group, never both. Mirrors the auth-method toggle; visibility is
// CSS-driven off the attribute.
function airouterToggleOAuthMode(sel) {
  const form = sel.closest(".provider-form");
  if (form) {
    form.setAttribute("data-oauth-mode", sel.value);
  }
}

// airouterSelectProviderCard marks the clicked card as the active one so the
// grid shows which recipe's add-form is open. Selection is purely visual and
// non-persistent; a page reload clears it, which matches the empty form slot on
// load.
function airouterSelectProviderCard(btn) {
  document.querySelectorAll(".provider-card.selected").forEach(function (c) {
    c.classList.remove("selected");
    c.setAttribute("aria-pressed", "false");
  });
  btn.classList.add("selected");
  btn.setAttribute("aria-pressed", "true");
}

// airouterCloseProviderForm dismisses the open add-form and clears the card
// selection together, so a closed form never leaves a stale-selected card.
function airouterCloseProviderForm() {
  const slot = document.getElementById("provider-form-slot");
  if (slot) {
    slot.innerHTML = "";
  }
  document.querySelectorAll(".provider-card.selected").forEach(function (c) {
    c.classList.remove("selected");
    c.setAttribute("aria-pressed", "false");
  });
}

// Provider Edit/Save/Archive/Restore/Delete all swap either the whole list
// (#provider-list) or a single row (#provider-<id>) with outerHTML, changing the
// document height. When it grows (Edit expands a row into a tall form) or shrinks
// (Save/Archive/Delete), the browser reclamps scroll and can jump the viewport to
// the bottom. Preserve the scroll position across these swaps instead of chasing
// any particular row - scrollIntoView would follow an archived row down into the
// bottom "Archived" section, which is exactly the jump we want to avoid.
let airouterProviderScrollY = null;

function airouterIsProviderSwap(t) {
  return !!t && (t.id === "provider-list" || /^provider-\d+$/.test(t.id));
}

document.body.addEventListener("htmx:beforeSwap", function (event) {
  if (airouterIsProviderSwap(event.detail.target)) {
    airouterProviderScrollY = window.scrollY;
  }
});

// Restore on afterSwap, not afterSettle: afterSwap runs synchronously in the same
// task as the DOM replacement, before the browser paints, so the clamped/jumped
// position never reaches the screen. afterSettle fires on a later timer, letting
// one intermediate frame paint first - the visible flicker.
document.body.addEventListener("htmx:afterSwap", function (event) {
  if (airouterProviderScrollY !== null && airouterIsProviderSwap(event.detail.target)) {
    window.scrollTo({ top: airouterProviderScrollY });
    airouterProviderScrollY = null;
  }
});

// airouterResetProviderForm restores the create form to a pristine state after a
// successful add. form.reset() only reverts native input values; the oauth
// connect region, the Check result, and any Refresh status were mutated by HTMX
// swaps and must be cleared back to their initial server-rendered markup by hand.
function airouterResetProviderForm(form) {
  form.reset();
  airouterToggleAuthMethod(form.querySelector(".auth-method"));
  form.querySelectorAll(".oauth-tokens input").forEach(function (i) {
    i.value = "";
  });
  const region = form.querySelector(".oauth-region");
  if (region) {
    region.innerHTML =
      '<span class="muted">Not connected. Fill the fields above and press Connect.</span>';
  }
  form.querySelectorAll(".check-result").forEach(function (el) {
    el.innerHTML = "";
  });
  form.querySelectorAll(".oauth-tokens .check").forEach(function (el) {
    el.remove();
  });
}
