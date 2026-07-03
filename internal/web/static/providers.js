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

// airouterApplyPreset copies the selected option's data-* config into the
// sibling oauth inputs. Choosing "Custom" leaves the fields as-is for manual
// entry. It also updates the form's base_url/protocol from the preset.
function airouterApplyPreset(sel) {
  const opt = sel.options[sel.selectedIndex];
  if (!opt || opt.dataset.custom) {
    return;
  }
  const fields = sel.closest(".oauth-fields");
  const form = sel.closest(".provider-form");
  const set = (root, name, val) => {
    if (val === undefined) return;
    const el = root && root.querySelector('[name="' + name + '"]');
    if (!el) return;
    if (el.type === "checkbox") {
      el.checked = val === "true";
    } else {
      el.value = val;
    }
  };
  set(fields, "auth_url", opt.dataset.auth_url);
  set(fields, "token_url", opt.dataset.token_url);
  set(fields, "client_id", opt.dataset.client_id);
  set(fields, "client_secret", opt.dataset.client_secret);
  set(fields, "scopes", opt.dataset.scopes);
  set(fields, "redirect_uri", opt.dataset.redirect_uri);
  set(fields, "pkce", opt.dataset.pkce);
  set(form, "base_url", opt.dataset.base_url);
  set(form, "protocol", opt.dataset.protocol);
}
