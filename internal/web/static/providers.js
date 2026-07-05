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
