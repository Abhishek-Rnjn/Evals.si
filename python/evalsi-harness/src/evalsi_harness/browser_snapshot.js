// Run in the page by browser.py: numbers the interactive elements (data-evalsi-ref="eN")
// and returns the tree as text. Plain DOM, no library: it works in any Chromium.
() => {
  document.querySelectorAll('[data-evalsi-ref]').forEach(e => e.removeAttribute('data-evalsi-ref'));
  let refs = 0, nodes = 0;
  const MAX_NODES = 4000;
  const lines = [];
  const clip = (s, n) => { s = (s || '').replace(/\s+/g, ' ').trim(); return s.length > n ? s.slice(0, n) + '...' : s; };
  const hidden = el => {
    if (el.hidden || el.getAttribute('aria-hidden') === 'true') return true;
    const s = getComputedStyle(el);
    return s.display === 'none' || s.visibility === 'hidden';
  };
  const INPUT_ROLE = {submit: 'button', button: 'button', reset: 'button', image: 'button',
    checkbox: 'checkbox', radio: 'radio', range: 'slider'};
  const TAG_ROLE = {a: 'link', button: 'button', select: 'combobox', textarea: 'textbox',
    h1: 'heading', h2: 'heading', h3: 'heading', h4: 'heading', h5: 'heading', h6: 'heading',
    img: 'img', ul: 'list', ol: 'list', li: 'listitem', table: 'table', tr: 'row', td: 'cell',
    th: 'columnheader', nav: 'navigation', main: 'main', form: 'form', dialog: 'dialog',
    option: 'option', summary: 'button', p: 'paragraph'};
  const roleOf = el => {
    const tag = el.tagName.toLowerCase();
    const explicit = el.getAttribute('role');
    if (explicit) return explicit.split(' ')[0];
    if (tag === 'a') return el.hasAttribute('href') ? 'link' : null;
    if (tag === 'input') {
      const t = (el.getAttribute('type') || 'text').toLowerCase();
      if (t === 'hidden') return null;
      return INPUT_ROLE[t] || 'textbox';
    }
    if (tag === 'select' && (el.multiple || el.size > 1)) return 'listbox';
    return TAG_ROLE[tag] || null;
  };
  const INTERACTIVE = new Set(['link', 'button', 'checkbox', 'radio', 'slider', 'textbox',
    'combobox', 'listbox', 'menuitem', 'tab', 'switch', 'searchbox']);
  const LEAF = new Set(['link', 'button', 'heading', 'option', 'cell', 'columnheader', 'menuitem', 'tab']);
  const labelOf = el => {
    if (el.id) {
      const l = document.querySelector('label[for="' + CSS.escape(el.id) + '"]');
      if (l) return l.innerText;
    }
    const wrap = el.closest('label');
    return wrap ? wrap.innerText : '';
  };
  const nameOf = (el, role) => {
    const aria = el.getAttribute('aria-label');
    if (aria) return clip(aria, 120);
    const by = el.getAttribute('aria-labelledby');
    if (by) {
      const t = by.split(/\s+/).map(i => (document.getElementById(i) || {}).innerText || '').join(' ');
      if (t.trim()) return clip(t, 120);
    }
    const tag = el.tagName.toLowerCase();
    if (tag === 'img') return clip(el.getAttribute('alt') || '', 120);
    if (tag === 'input' || tag === 'select' || tag === 'textarea') {
      if (role === 'button') return clip(el.value || el.getAttribute('alt') || el.title, 120);
      return clip(labelOf(el) || el.getAttribute('placeholder') || el.title || el.name, 120);
    }
    // A container (navigation, form, list) is named only by its label, not its text,
    // which its children show anyway.
    if (!LEAF.has(role)) return clip(el.title, 120);
    return clip(el.innerText || el.textContent, 120) || clip(el.title, 120);
  };
  const walk = (el, depth) => {
    if (++nodes > MAX_NODES) return;
    for (const child of el.childNodes) {
      if (nodes > MAX_NODES) return;
      if (child.nodeType === 3) {
        const t = clip(child.textContent, 200);
        if (t) lines.push('  '.repeat(depth) + '- text "' + t + '"');
        continue;
      }
      if (child.nodeType !== 1) continue;
      const tag = child.tagName.toLowerCase();
      if (['script', 'style', 'noscript', 'template', 'svg', 'head'].includes(tag)) continue;
      if (hidden(child)) continue;
      const role = roleOf(child);
      if (!role) { walk(child, depth); continue; }
      let line = '  '.repeat(depth) + '- ' + role;
      const name = nameOf(child, role);
      if (name) line += ' "' + name.replace(/"/g, "'") + '"';
      if (role === 'heading') line += ' [level=' + (tag[1] || child.getAttribute('aria-level') || '') + ']';
      if (child.disabled) line += ' [disabled]';
      if (child.checked) line += ' [checked]';
      if (child.selected) line += ' [selected]';
      if (role === 'textbox' && child.value) line += ' value="' + clip(child.value, 80).replace(/"/g, "'") + '"';
      if (role === 'combobox' || role === 'listbox') {
        const chosen = Array.from(child.selectedOptions || []).map(o => clip(o.text, 60));
        if (chosen.length) line += ' value="' + chosen.join(', ').replace(/"/g, "'") + '"';
      }
      if (role === 'link' && child.getAttribute('href')) line += ' [href=' + clip(child.getAttribute('href'), 150) + ']';
      if (INTERACTIVE.has(role) && !child.disabled) {
        refs += 1;
        child.setAttribute('data-evalsi-ref', 'e' + refs);
        line += ' [ref=e' + refs + ']';
      }
      lines.push(line);
      if (role === 'combobox' || role === 'listbox') {
        for (const o of child.querySelectorAll('option')) {
          lines.push('  '.repeat(depth + 1) + '- option "' + clip(o.text, 80).replace(/"/g, "'") + '"' + (o.selected ? ' [selected]' : ''));
        }
      } else if (!LEAF.has(role) && role !== 'img' && role !== 'textbox') {
        walk(child, depth + 1);
      }
    }
  };
  walk(document.body || document.documentElement, 0);
  if (nodes > MAX_NODES) lines.push('- (the page is larger than the snapshot; the rest is not shown)');
  return lines.join('\n');
}
