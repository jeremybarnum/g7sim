package main

// indexHTML is the control panel served at /. It is self-contained (no
// external assets, so it works on a Pi with no internet) and drives the same
// JSON API documented in api.go.
const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>G7 sensor emulator</title>
<style>
  :root { color-scheme: dark; --bg:#14161a; --card:#1d2026; --line:#2b2f37; --fg:#e8eaed; --mut:#9aa0aa; --accent:#4f9cf8; --ok:#43c06d; --warn:#e0a53b; --bad:#e2574c; }
  * { box-sizing:border-box; }
  body { margin:0; font:15px/1.4 -apple-system,Segoe UI,Roboto,sans-serif; background:var(--bg); color:var(--fg); }
  header { padding:16px; border-bottom:1px solid var(--line); display:flex; flex-wrap:wrap; gap:4px 20px; align-items:baseline; }
  header h1 { font-size:18px; margin:0; }
  header .meta { color:var(--mut); font-size:13px; }
  main { max-width:760px; margin:0 auto; padding:16px; display:grid; gap:16px; }
  .card { background:var(--card); border:1px solid var(--line); border-radius:10px; padding:16px; }
  .card h2 { font-size:13px; text-transform:uppercase; letter-spacing:.05em; color:var(--mut); margin:0 0 12px; }
  .glucose { display:flex; align-items:baseline; gap:12px; }
  .glucose .val { font-size:44px; font-weight:600; }
  .glucose .unit { color:var(--mut); }
  .glucose .state { margin-left:auto; padding:3px 10px; border-radius:20px; font-size:13px; background:#2b2f37; }
  .state.ok { background:rgba(67,192,109,.18); color:var(--ok); }
  .state.warn { background:rgba(224,165,59,.18); color:var(--warn); }
  .state.bad { background:rgba(226,87,76,.18); color:var(--bad); }
  label { display:block; font-size:13px; color:var(--mut); margin:10px 0 4px; }
  input[type=number], input[type=text], select { width:100%; padding:8px; background:#14161a; border:1px solid var(--line); border-radius:7px; color:var(--fg); font-size:15px; }
  input[type=range] { width:100%; }
  .row { display:flex; gap:12px; flex-wrap:wrap; }
  .row > div { flex:1; min-width:120px; }
  .seg { display:flex; gap:8px; }
  .seg button { flex:1; }
  button { padding:9px 14px; background:#2b2f37; color:var(--fg); border:1px solid var(--line); border-radius:7px; font-size:14px; cursor:pointer; }
  button:hover { border-color:var(--accent); }
  button.active { background:var(--accent); border-color:var(--accent); color:#0b0d10; font-weight:600; }
  button:disabled { opacity:.45; cursor:default; }
  button.primary:disabled { background:#2b2f37; border-color:var(--line); color:var(--mut); opacity:1; }
  button.primary { background:var(--accent); border-color:var(--accent); color:#0b0d10; font-weight:600; }
  button.danger:hover { border-color:var(--bad); }
  table { width:100%; border-collapse:collapse; font-size:14px; }
  th, td { text-align:left; padding:6px 8px; border-bottom:1px solid var(--line); }
  th { color:var(--mut); font-weight:500; }
  .muted { color:var(--mut); font-size:13px; }
  .hidden { display:none; }
  footer { max-width:760px; margin:0 auto; padding:4px 16px 24px; color:var(--mut); font-size:12px; }
</style>
</head>
<body>
<header>
  <h1 id="name">G7 sensor</h1>
  <span class="meta" id="model"></span>
  <span class="meta" id="serial"></span>
  <span class="meta" id="code"></span>
  <span class="meta" id="age"></span>
</header>
<main>
  <div class="card">
    <h2>Current reading</h2>
    <div class="glucose">
      <span class="val" id="glucose">—</span>
      <span class="unit">mg/dL</span>
      <span id="trend" class="unit"></span>
      <span class="state" id="statelabel">—</span>
    </div>
    <div class="muted" id="window" style="margin-top:10px"></div>
  </div>

  <div class="card">
    <h2>Glucose</h2>
    <div class="seg" id="kind">
      <button data-kind="fixed">Fixed</button>
      <button data-kind="sine">Sine</button>
      <button data-kind="walk">Random walk</button>
    </div>
    <div class="row">
      <div>
        <label id="valueLabel">Value (mg/dL)</label>
        <input type="number" id="value" min="40" max="400" step="1" value="120">
        <input type="range" id="valueRange" min="40" max="400" step="1" value="120">
      </div>
    </div>
    <div class="row" id="sineOpts">
      <div><label>Amplitude</label><input type="number" id="amplitude" min="0" max="180" step="5" value="50"></div>
      <div><label>Period (min)</label><input type="number" id="period" min="10" max="1440" step="10" value="240"></div>
    </div>
    <div style="margin-top:14px; display:flex; align-items:center; gap:12px">
      <button class="primary" id="applyGlucose" disabled>Apply</button>
      <span class="muted" id="applyMsg"></span>
    </div>
    <div class="muted" id="activeModel" style="margin-top:10px"></div>
    <div class="muted" style="margin-top:4px">New settings take effect on the next 5-minute reading.</div>
  </div>

  <div class="card">
    <h2>Sensor state / error</h2>
    <label>Algorithm state reported in every reading</label>
    <select id="algostate">
      <option value="0">Normal (use glucose model)</option>
      <option value="2">Warmup (no glucose)</option>
      <option value="3">Excess noise</option>
      <option value="7">Needs calibration</option>
      <option value="18">Temporary sensor issue</option>
      <option value="15">Session expired</option>
      <option value="24">Expired</option>
      <option value="25">Sensor failed</option>
      <option value="1">Stopped</option>
    </select>
    <div class="muted" style="margin-top:8px">Any non-normal state is sent as-is; the error states report no glucose.</div>
  </div>

  <div class="card">
    <h2>Display slots</h2>
    <table><thead><tr><th>Type</th><th>Held</th><th>Last seen</th><th>Misses</th><th>Muted</th></tr></thead>
    <tbody id="slots"><tr><td colspan="5" class="muted">none yet</td></tr></tbody></table>
    <div style="margin-top:14px"><button id="muteWatch">Mute the watch</button></div>
    <div class="muted" style="margin-top:6px">A muted display is refused every reading, as if out of range; the others still collect. Ends when unmuted or the emulator restarts.</div>
    <div style="margin-top:14px"><button class="danger" id="forget">Forget all keys</button></div>
    <div class="muted" style="margin-top:6px">Drops every display's key, so the next connection pairs again.</div>
  </div>

  <div class="card">
    <h2>Start a new sensor</h2>
    <div class="row">
      <div><label>Model</label><select id="nsModel">
        <option value="g7">G7 (DXCM)</option><option value="oneplus">ONE+ (DX02)</option><option value="stelo">Stelo (DX01)</option>
      </select></div>
      <div><label>Preset</label><select id="nsDays"><option value="10">10 day</option><option value="15">15 day</option></select></div>
    </div>
    <div class="row">
      <div><label>Lifetime (hours, before the 12 h grace)</label><input type="number" id="nsLifetime" min="1" step="1"></div>
      <div><label>Warmup (minutes)</label><input type="number" id="nsWarmup" min="0" max="1092" step="1"></div>
    </div>
    <div class="row">
      <div><label>Activated (minutes ago; 0 starts warmup)</label><input type="number" id="nsAge" min="0" step="1" value="120"></div>
      <div><label>Pairing code</label><input type="text" id="nsCode" inputmode="numeric" maxlength="4" placeholder="random"></div>
      <div><label>Serial</label><input type="text" id="nsSerial" inputmode="numeric" placeholder="random"></div>
    </div>
    <div style="margin-top:14px; display:flex; align-items:center; gap:12px">
      <button class="danger" id="nsStart">Start new sensor</button>
      <span class="muted" id="nsMsg"></span>
    </div>
    <div class="muted" style="margin-top:6px">Replaces this sensor and restarts the emulator; a phone paired to it must pair the new one. Lifetime and warmup default to the preset.</div>
  </div>
</main>
<footer>For development use only — not a Dexcom product. This sensor emits simulated, made-up data; never use it for therapy.</footer>
<script>
const TYPE = {1:"medical",2:"phone",3:"watch",4:"receiver",5:"pump"};
const STATE = {2:["warmup","warn"],3:["excess noise","warn"],6:["ok","ok"],7:["needs calibration","warn"],
  15:["session expired","bad"],18:["temporary issue","warn"],24:["expired","bad"],25:["sensor failed","bad"],1:["stopped","bad"]};
let kind = "sine", touched = false, serverMode = {}, watchMuted = false;

function formDirty(){
  const v=+document.getElementById('value').value, st=+document.getElementById('algostate').value;
  if(kind !== (serverMode.kind||'sine')) return true;
  if(v !== serverMode.value) return true;
  if(st !== (serverMode.algorithmState||0)) return true;
  if(kind==='sine'){
    if(+document.getElementById('amplitude').value !== serverMode.amplitude) return true;
    if(+document.getElementById('period').value !== serverMode.periodMinutes) return true;
  }
  return false;
}
function updateApply(){ document.getElementById('applyGlucose').disabled = !formDirty(); }

function ago(iso){ if(!iso) return "—"; const s=(Date.now()-new Date(iso))/1000;
  if(s<60) return Math.round(s)+"s ago"; if(s<3600) return Math.round(s/60)+"m ago"; return Math.round(s/3600)+"h ago"; }

function setKind(k){ kind=k; document.querySelectorAll('#kind button').forEach(b=>b.classList.toggle('active',b.dataset.kind===k));
  document.getElementById('sineOpts').classList.toggle('hidden',k!=='sine');
  document.getElementById('valueLabel').textContent = k==='sine' ? 'Mean (mg/dL)' : k==='walk' ? 'Target (mg/dL)' : 'Value (mg/dL)'; }

document.querySelectorAll('#kind button').forEach(b=>b.onclick=()=>{ setKind(b.dataset.kind); touched=true; updateApply(); });
document.getElementById('value').oninput = e=>{ document.getElementById('valueRange').value=e.target.value; touched=true; updateApply(); };
document.getElementById('valueRange').oninput = e=>{ document.getElementById('value').value=e.target.value; touched=true; updateApply(); };
document.getElementById('amplitude').oninput = ()=>{ touched=true; updateApply(); };
document.getElementById('period').oninput = ()=>{ touched=true; updateApply(); };

async function applyGlucose(){
  const msg = document.getElementById('applyMsg');
  const body = { kind,
    value: +document.getElementById('value').value,
    amplitude: +document.getElementById('amplitude').value,
    periodMinutes: +document.getElementById('period').value,
    algorithmState: +document.getElementById('algostate').value };
  msg.textContent = 'Applying…';
  try {
    const r = await fetch('/glucose',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
    if(!r.ok) throw new Error(r.status);
    touched=false; await refresh();
    msg.textContent = 'Applied ✓';
    setTimeout(()=>{ if(msg.textContent==='Applied ✓') msg.textContent=''; }, 2500);
  } catch(e){ msg.textContent = 'Failed — '+e.message; }
}
document.getElementById('applyGlucose').onclick = applyGlucose;
document.getElementById('algostate').onchange = ()=>{ touched=true; updateApply(); };
document.getElementById('forget').onclick = async()=>{ await fetch('/forget',{method:'POST'}); refresh(); };
document.getElementById('muteWatch').onclick = async()=>{
  await fetch('/mute',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({displayType:3,muted:!watchMuted})}); refresh(); };

async function refresh(){
  let s; try { s = await (await fetch('/status')).json(); } catch(e){ return; }
  document.getElementById('name').textContent = s.name || 'G7 sensor';
  const h = s.lifetimeHours;
  document.getElementById('model').textContent = ({g7:'G7',oneplus:'ONE+',stelo:'Stelo'}[s.model]||s.model)+' · '
    +(h%24===0 ? h/24+' day' : h+' h')+' · warmup '+Math.round(s.warmupMinutes)+' min';
  document.getElementById('serial').textContent = 'serial '+s.serial;
  document.getElementById('code').textContent = 'code '+s.code;
  document.getElementById('age').textContent = s.activation ? 'activated '+ago(s.activation) : '';
  const r = s.latest || {};
  document.getElementById('glucose').textContent = (r.glucose!=null) ? r.glucose : '—';
  document.getElementById('trend').textContent = (r.trend!=null) ? (r.trend>0?'+':'')+r.trend.toFixed(1)+'/min' : '';
  const st = STATE[r.state] || ['state '+(r.state??'?'),''];
  const lab = document.getElementById('statelabel'); lab.textContent = st[0]; lab.className = 'state '+st[1];
  if(s.window){ const served=(s.window.servedTypes||[]).length; document.getElementById('window').textContent =
    'window '+s.window.index+' · '+s.window.openLinks+' open link(s) · '+served+' collected'; }
  const g = s.glucoseMode || {};
  serverMode = g;
  let desc = g.kind==='fixed' ? 'fixed at '+g.value
    : g.kind==='walk' ? 'random walk toward '+g.value
    : 'sine, mean '+g.value+', amp '+g.amplitude+', '+g.periodMinutes+' min';
  if(g.algorithmState) desc += ' · state '+g.algorithmState;
  document.getElementById('activeModel').textContent = 'Sensor is using: '+desc;
  if(!touched){ setKind(g.kind||'sine');
    if(g.value!=null){ document.getElementById('value').value=g.value; document.getElementById('valueRange').value=g.value; }
    if(g.amplitude!=null) document.getElementById('amplitude').value=g.amplitude;
    if(g.periodMinutes!=null) document.getElementById('period').value=g.periodMinutes;
    document.getElementById('algostate').value = g.algorithmState||0; }
  updateApply();
  const muted = s.muted||[]; watchMuted = muted.includes(3);
  document.getElementById('muteWatch').textContent = watchMuted ? 'Unmute the watch' : 'Mute the watch';
  const tb = document.getElementById('slots'); const keys = Object.keys(s.slots||{});
  tb.innerHTML = keys.length ? keys.map(k=>{const sl=s.slots[k];
    return '<tr><td>'+(TYPE[k]||k)+'</td><td>'+(sl.held?'yes':'no')+'</td><td>'+ago(sl.lastSeen)+'</td><td>'+sl.misses+'</td><td>'+(muted.includes(+k)?'yes':'no')+'</td></tr>';}).join('')
    : '<tr><td colspan="5" class="muted">none yet</td></tr>';
}
const PRESET = {10:{lifetime:240, warmup:27}, 15:{lifetime:360, warmup:62}};
const field = id=>document.getElementById(id);
function showPreset(){ const p = PRESET[field('nsDays').value];
  field('nsLifetime').placeholder = p.lifetime; field('nsWarmup').placeholder = p.warmup; }
field('nsModel').onchange = e=>{ field('nsDays').value = e.target.value==='stelo' ? '15' : '10'; showPreset(); };
field('nsDays').onchange = showPreset;
field('nsStart').onclick = async()=>{
  const v = id=>field(id).value.trim();
  const num = id=>v(id)==='' ? undefined : +v(id);
  const body = { model:v('nsModel'), days:+v('nsDays'), code:v('nsCode'), serial:num('nsSerial')||0,
    ageMinutes:num('nsAge'), lifetimeHours:num('nsLifetime'), warmupMinutes:num('nsWarmup') };
  if(!confirm('Replace '+field('name').textContent+' with a new sensor?')) return;
  const msg = field('nsMsg'); msg.textContent = 'Starting…';
  try {
    const r = await fetch('/new',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
    const t = await r.text();
    if(!r.ok) throw new Error(t.trim());
    const n = JSON.parse(t);
    msg.textContent = 'Restarting as '+n.name+', code '+n.code+'…';
    setTimeout(()=>{ if(msg.textContent.startsWith('Restarting')) msg.textContent=''; }, 15000);
  } catch(e){ msg.textContent = 'Failed — '+e.message; }
};
showPreset();
setKind('sine'); refresh(); setInterval(refresh, 2000);
</script>
</body>
</html>`
