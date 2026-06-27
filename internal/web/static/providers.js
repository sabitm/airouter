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
