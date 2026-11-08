package dashboard

// statusHTML is the whole page: one card, four rows, no dependencies.
//
// THE DESIGN IS POCKET'S (B57.14), via internal/brand: white page, cloud card,
// ink text, one tangerine rule; Bricolage Grotesque for the heading and Figtree
// for the UI and every number, both named first over the system stack and never
// fetched. Light is primary, dark follows the viewer's preference. The flat mark sits beside the name; it is the brand's own
// file, inlined, never redrawn. The layout is a dense stack of hairline-separated
// rows, label left and value right. status_test.go pins the hexes, so a drift
// from the brand fails the build rather than quietly diverging.
//
// THE ONE INVARIANT WORTH NAMING: text is never a hue. The health state is a
// word in ink beside a small coloured dot — the dot carries the status colour,
// the word never does. That is why this page has no green "Healthy" or red
// "Down" label.
//
// It is entirely self-contained: no CDN, no web font, no analytics. The API host
// must not make a visitor's browser talk to a third party.
//
// {{BRAND_CSS}}, {{MARK}} and {{VERSION}} are filled in once, by New.
const statusHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>Talyvor Lens</title>
<style>
{{BRAND_CSS}}
*{box-sizing:border-box}
html,body{height:100%}
body{
  margin:0; background:var(--tv-canvas); color:var(--tv-ink);
  font-family:var(--tv-font-sans); font-size:15px; line-height:24px;
  -webkit-font-smoothing:antialiased; text-rendering:optimizeLegibility;
  display:flex; align-items:flex-start; justify-content:center; padding:48px 16px;
}
main{width:100%; max-width:560px}
.num{font-variant-numeric:tabular-nums}
.card{background:var(--tv-surface); border:1px solid var(--tv-line); border-radius:var(--tv-radius-md); overflow:hidden}
.head{display:flex; align-items:flex-start; gap:14px; padding:20px 16px 18px; border-bottom:1px solid var(--tv-line)}
.head .tv-mark{width:32px; height:32px; margin-top:2px}
.title{flex:1; min-width:0}
h1{margin:0; font-size:22px; line-height:26px; font-weight:750; letter-spacing:-.015em; color:var(--tv-ink)}
.sub{margin:2px 0 12px; font-size:13px; line-height:20px; color:var(--tv-ink-muted)}
.ver{font-size:12px; line-height:1; font-weight:500; color:var(--tv-ink-muted); margin-top:4px;
     border:1px solid var(--tv-line-strong); border-radius:var(--tv-radius-pill); padding:5px 9px; white-space:nowrap}
.row{display:flex; align-items:center; justify-content:space-between; gap:16px;
     min-height:52px; padding:10px 16px; border-bottom:1px solid var(--tv-line)}
.row:last-child{border-bottom:0}
.label{font-size:15px; line-height:22px; color:var(--tv-ink)}
.hint{font-size:13px; line-height:20px; color:var(--tv-ink-muted)}
.val{font-size:14px; color:var(--tv-ink); display:flex; align-items:center; gap:8px; white-space:nowrap}
.dot{width:7px; height:7px; border-radius:var(--tv-radius-pill); background:var(--tv-ink-muted); flex:none}
.dot.ok{background:var(--tv-positive)} .dot.warn{background:var(--tv-caution)} .dot.bad{background:var(--tv-critical)}
.note{padding:16px; border-bottom:1px solid var(--tv-line); background:var(--tv-raised)}
.note p{margin:0; font-size:14px; line-height:22px; color:var(--tv-ink-muted)}
.note p + p{margin-top:8px}
a.row{text-decoration:none; color:inherit}
a.row:hover{background:var(--tv-raised)}
a.row .go{font-size:18px; color:var(--tv-ink)}
a.row:focus-visible{outline:2px solid var(--tv-ink); outline-offset:-2px}
footer{margin-top:14px; text-align:center; font-size:13px; color:var(--tv-ink-muted)}
@media (prefers-reduced-motion: reduce){
  *,*::before,*::after{animation-duration:.001ms!important; transition-duration:.001ms!important}
}
</style>
</head>
<body>
<main>
  <div class="card">
    <div class="head">
      {{MARK}}
      <div class="title">
        <h1>Talyvor Lens</h1>
        <p class="sub">Agent Wallets and the gateway that enforces them</p>
        <span class="tv-rule"></span>
      </div>
      <span class="ver num">v{{VERSION}}</span>
    </div>

    <div class="row">
      <div>
        <div class="label">Service</div>
        <div class="hint">Live, read from /healthz</div>
      </div>
      <div class="val" id="health" aria-live="polite"><span class="dot" id="dot"></span><span id="health-word">Checking…</span></div>
    </div>

    <div class="row">
      <div>
        <div class="label">Running for</div>
        <div class="hint">Since this instance last started</div>
      </div>
      <div class="val num" id="uptime">—</div>
    </div>

    <div class="note">
      <p>Every AI agent gets a wallet — a balance, spending rules, approvals, a card and
         a live statement — and this host is where its rules are enforced: Lens judges each
         model call and payment against the agent's wallet before it happens.</p>
      <p>It is an API, not a user interface — there is nothing to sign in to here.</p>
      <p>No account data appears on this page: it holds no credential, by design. Your
         workspace, usage and billing live in the app.</p>
    </div>

    <a class="row" href="/status">
      <div>
        <div class="label">Service status</div>
        <div class="hint">Per-component health and provider latency</div>
      </div>
      <span class="go" aria-hidden="true">&rsaquo;</span>
    </a>

    <a class="row" href="https://app.talyvor.com">
      <div>
        <div class="label">Wallet console</div>
        <div class="hint">Your agents, their rules and statements — app.talyvor.com</div>
      </div>
      <span class="go" aria-hidden="true">&rsaquo;</span>
    </a>

    <a class="row" href="/openapi.json">
      <div>
        <div class="label">API reference</div>
        <div class="hint">OpenAPI — the agent wallet routes first</div>
      </div>
      <span class="go" aria-hidden="true">&rsaquo;</span>
    </a>

    <a class="row" href="https://app.talyvor.com/documentation">
      <div>
        <div class="label">Documentation</div>
        <div class="hint">app.talyvor.com/documentation</div>
      </div>
      <span class="go" aria-hidden="true">&rsaquo;</span>
    </a>
  </div>
  <footer>Talyvor Lens <span class="num">v{{VERSION}}</span></footer>
</main>
<script>
// The page's only live reading. /healthz is unauthenticated, so this is the one
// thing the page can show truthfully without a credential — and it is fetched,
// never painted in. A failure says so plainly rather than leaving a stale word.
(function () {
  var word = document.getElementById('health-word');
  var dot = document.getElementById('dot');
  var up = document.getElementById('uptime');

  function human(sec) {
    sec = Math.max(0, Math.floor(sec || 0));
    var d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
    if (d) { return d + 'd ' + h + 'h'; }
    if (h) { return h + 'h ' + m + 'm'; }
    return m + 'm';
  }

  function paint(status, uptimeSeconds) {
    // Deliberately the OVERALL word only. The per-check detail (database, cache,
    // replica) is in /healthz for an operator who asks for it; enumerating a
    // host's internals to every passer-by is not this page's job.
    var map = { healthy: 'ok', degraded: 'warn', unhealthy: 'bad' };
    dot.className = 'dot ' + (map[status] || '');
    word.textContent = status ? status.charAt(0).toUpperCase() + status.slice(1) : 'Unknown';
    up.textContent = uptimeSeconds === null ? '—' : human(uptimeSeconds);
  }

  function load() {
    fetch('/healthz', { headers: { 'Accept': 'application/json' }, cache: 'no-store' })
      .then(function (r) { return r.json(); })
      .then(function (d) { paint(String(d.status || ''), d.uptime_seconds); })
      .catch(function () {
        dot.className = 'dot bad';
        word.textContent = 'Unreachable';
        up.textContent = '—';
      });
  }
  load();
  setInterval(load, 30000);
})();
</script>
</body>
</html>
`
