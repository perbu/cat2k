// API base URL - assumes API is served on same host
const API_BASE = window.location.origin;

// Per-tracker palettes. Each spans multiple hues so intensity is readable
// within a single cat's heatmap. Combined with mix-blend-mode: screen on the
// heat-layer canvases, overlap zones mix toward white.
const HEATMAP_PALETTES = [
    // Cool: deep blue → cyan-blue → mint-cyan → pale cyan
    { 0.0: '#0040c0', 0.4: '#00b4ff', 0.7: '#40ffc0', 1.0: '#e0ffff' },
    // Warm: dark red → orange-red → orange → yellow (mirrors classic heat gradient)
    { 0.0: '#c01818', 0.4: '#ff4818', 0.7: '#ffa020', 1.0: '#ffe830' }
];

// The trail shows everything since 06:00 local this morning — the cat's day
// so far. (status.html's radar keeps its own rolling 3h server-side window.)
const TRAIL_START_HOUR = 6;

// Trail fade is by absolute age, not window size: full strength for the last
// hour, then fading out over TRAIL_FADE_HOURS.
const TRAIL_FADE_HOURS = 12;

// Hourly dots mark the fix nearest each whole hour, if within this tolerance.
const HOUR_DOT_TOLERANCE_MIN = 15;

// Initial map zoom when data first loads (Leaflet levels: higher = closer;
// OSM tiles go to 19, the heat layer to 17). Starts zoomed in on the cats.
const DEFAULT_ZOOM = 17;

// Per-cat accent colours (DESIGN.md). Indexed in the same id-sorted order as
// the heatmap palettes, so cat #0 (Bella) is green and cat #1 (Luna) is orange.
// Used ONLY in the control panel — the cat's own dot + activity chart. The map
// keeps its blue/red heatmap palettes (see HEATMAP_PALETTES above).
const CAT_ACCENTS = [
    { color: '#17BE7E', muted: '#BEEBD6' }, // Bella
    { color: '#F5991F', muted: '#FBDDB4' }  // Luna
];

function accentForIndex(idx) {
    return CAT_ACCENTS[idx % CAT_ACCENTS.length];
}

// State
let map = null;
let heatLayers = {};         // trackerId -> L.heatLayer
let trailLayers = {};        // trackerId -> L.layerGroup of polyline segments
let latestMarkers = {};      // trackerId -> L.marker
let allTrackers = [];
let currentDateRange = { days: 7 };
let positionsByTracker = {}; // trackerId -> positions[]
let activityData = null;     // most recent /api/activity response
let pois = [];               // points of interest from /api/status
let heatMapSettings = {
    auto: true,              // "Auto-tune" toggle — derive from zoom/density
    intensity: 0.3,
    radius: 25,
    blur: 15
};
let waypointsByTracker = {};   // trackerId -> waypoints[] from /api/waypoints
let transitionsByTracker = {}; // trackerId -> edges[] from /api/waypoints/transitions
let waypointMarkers = {};      // trackerId -> L.layerGroup of waypoint markers
let routeLayers = {};          // trackerId -> L.layerGroup of transition edges
let waypointSettings = {
    places: true,              // waypoint markers on the map
    routes: false              // transition-graph overlay
};
let rangeByTracker = {};       // trackerId -> /api/range response
let rangeLayers = {};          // trackerId -> L.layerGroup of contour polygons
let rangeSettings = {
    enabled: true,             // KDE contour layer on the map
    cats: {},                  // trackerId -> bool (absent = on)
    month: null                // YYYY-MM, initialized to the current month
};
let overlapSeries = [];        // /api/range/overlap response
let presenceByTracker = {};    // trackerId -> /api/presence response
let excursionsByTracker = {};  // trackerId -> /api/presence/excursions .stats
let behaviorSegByTracker = {};   // trackerId -> /api/behavior/segments response
let behaviorBudgetByTracker = {}; // trackerId -> /api/behavior/budget response
let behaviorMatrixByTracker = {}; // trackerId -> /api/behavior/matrix response
let behaviorLayers = {};         // trackerId -> L.layerGroup of state-colored steps
let behaviorSettings = {
    tracks: false                // color tracks by rest/forage/transit state
};

function paletteForIndex(idx) {
    return HEATMAP_PALETTES[idx % HEATMAP_PALETTES.length];
}

// Palette index for a tracker, keyed on the full id-sorted roster. Single source
// of truth so the same cat gets the same color in the legend, activity panel,
// and map — even when a tracker has no positions in the current range (in which
// case the map skips it, but its color slot must not shift).
function trackerPaletteIndex(id) {
    const ids = allTrackers.map(t => t.id).sort((a, b) => a - b);
    const i = ids.indexOf(id);
    return i < 0 ? 0 : i;
}

function paletteSwatch(palette) {
    // Pick a mid-to-high stop so the swatch matches what the heatmap actually shows
    const stops = Object.keys(palette).map(Number).sort((a, b) => a - b);
    const mid = stops[Math.floor(stops.length * 0.7)] ?? stops[stops.length - 1];
    return palette[mid] ?? '#888';
}

// Parse a #rrggbb color to RGB components (ported from status.html).
function hexToRgb(hex) {
    const result = /^#?([a-f\d]{2})([a-f\d]{2})([a-f\d]{2})$/i.exec(hex);
    return result
        ? { r: parseInt(result[1], 16), g: parseInt(result[2], 16), b: parseInt(result[3], 16) }
        : { r: 255, g: 255, b: 255 };
}

// Style for one trail segment, faded by the absolute age of its midpoint.
// Fixes from the last hour show the tracker's color at full strength; older
// ones fade toward dark and translucent so the freshest path reads clearly.
function trailSegmentStyle(baseHex, midTimeMs) {
    const ageHours = (Date.now() - midTimeMs) / (1000 * 60 * 60);
    const fade = Math.min(Math.max((ageHours - 1) / (TRAIL_FADE_HOURS - 1), 0), 1);
    const rgb = hexToRgb(baseHex);
    const r = Math.round(rgb.r * (1 - fade));
    const g = Math.round(rgb.g * (1 - fade));
    const b = Math.round(rgb.b * (1 - fade));
    return {
        color: `rgb(${r}, ${g}, ${b})`,
        opacity: 0.85 - fade * 0.55, // 0.85 (fresh) -> 0.30 (oldest)
        weight: 3,
        lineCap: 'round'
    };
}

// One point on the Catmull-Rom spline through p1->p2, with p0/p3 as the
// neighboring control points. t in [0,1]. Operates component-wise on [lat,lng].
// Trades positional accuracy for smoother corners — the curve still passes
// through every real fix, only the angles between them are rounded off.
function catmullRom(p0, p1, p2, p3, t) {
    const t2 = t * t;
    const t3 = t2 * t;
    const f = (a, b, c, d) =>
        0.5 * ((2 * b) + (-a + c) * t + (2 * a - 5 * b + 4 * c - d) * t2 + (-a + 3 * b - 3 * c + d) * t3);
    return [f(p0[0], p1[0], p2[0], p3[0]), f(p0[1], p1[1], p2[1], p3[1])];
}

// Subdivide segment pts[i]->pts[i+1] into a smooth curve, clamping control
// points at the ends. Returns TRAIL_SPLINE_STEPS+1 [lat,lng] points.
const TRAIL_SPLINE_STEPS = 12;
function splineSegment(pts, i) {
    const p0 = pts[i - 1] || pts[i];
    const p1 = pts[i];
    const p2 = pts[i + 1];
    const p3 = pts[i + 2] || pts[i + 1];
    const out = [];
    for (let s = 0; s <= TRAIL_SPLINE_STEPS; s++) {
        out.push(catmullRom(p0, p1, p2, p3, s / TRAIL_SPLINE_STEPS));
    }
    return out;
}

function makePinIcon(color) {
    const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="26" height="38" viewBox="0 0 26 38">
        <path d="M13 0C5.8 0 0 5.8 0 13c0 9.75 13 25 13 25s13-15.25 13-25C26 5.8 20.2 0 13 0z"
              fill="${color}" stroke="#222" stroke-width="1.5"/>
        <circle cx="13" cy="13" r="5" fill="#fff"/>
    </svg>`;
    return L.divIcon({
        html: svg,
        className: 'cat-pin-icon',
        iconSize: [26, 38],
        iconAnchor: [13, 38],
        popupAnchor: [0, -34]
    });
}

// Initialize date inputs with defaults
function initializeDateInputs() {
    const end = new Date();
    const start = new Date();
    start.setDate(start.getDate() - 7);

    document.getElementById('end-date').valueAsDate = end;
    document.getElementById('start-date').valueAsDate = start;
}

// Get current date range
function getDateRange() {
    if (currentDateRange.days) {
        const end = new Date();
        const start = new Date();
        start.setDate(start.getDate() - currentDateRange.days);
        return { start, end };
    } else {
        return {
            start: new Date(document.getElementById('start-date').value),
            end: new Date(document.getElementById('end-date').value)
        };
    }
}

// Show/hide loading indicator
function setLoading(isLoading) {
    const loadingEl = document.getElementById('loading');
    const controls = document.querySelectorAll('select, button, input');

    if (isLoading) {
        loadingEl.classList.remove('hidden');
        controls.forEach(el => el.disabled = true);
    } else {
        loadingEl.classList.add('hidden');
        controls.forEach(el => el.disabled = false);
    }
}

// Show error message
function showError(message) {
    const errorEl = document.getElementById('error');
    errorEl.textContent = message;
    errorEl.classList.add('show');
    setTimeout(() => {
        errorEl.classList.remove('show');
    }, 5000);
}

function sortedTrackerIds() {
    return Object.keys(positionsByTracker).map(Number).sort((a, b) => a - b);
}

// Update the header caption and slim footer: position count, last-update time,
// and the range start date. These readouts used to live in a "Statistics" block;
// the redesign folds them into the chrome (DESIGN.md §3).
function updateMeta() {
    const range = getDateRange();
    const ids = sortedTrackerIds();

    let total = 0;
    let latestOverall = null;

    ids.forEach(id => {
        const positions = positionsByTracker[id] || [];
        total += positions.length;
        if (positions.length > 0) {
            const t = new Date(positions[0].timestamp);
            if (!latestOverall || t > latestOverall) latestOverall = t;
        }
    });

    const countStr = total ? total.toLocaleString() : '—';
    const positionsStr = total ? `${countStr} positions` : '—';

    const timeEl = document.getElementById('update-time');
    if (timeEl) {
        timeEl.textContent = latestOverall
            ? `Updated ${latestOverall.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' })}`
            : 'Updated —';
    }
    setText('update-count', positionsStr);
    setText('footer-count', positionsStr);

    if (range.start) {
        const sinceStr = range.start.toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' });
        setText('footer-since', `since ${sinceStr}`);
    }
}

function setText(id, text) {
    const el = document.getElementById(id);
    if (el) el.textContent = text;
}

// Fetch trackers from API
async function fetchTrackers() {
    try {
        const response = await fetch(`${API_BASE}/api/trackers`);
        if (!response.ok) {
            throw new Error(`Failed to fetch trackers: ${response.statusText}`);
        }
        const data = await response.json();
        return data.trackers || [];
    } catch (error) {
        console.error('Error fetching trackers:', error);
        throw error;
    }
}

// Fetch positions for a tracker
async function fetchPositions(trackerID, start, end) {
    const params = new URLSearchParams({
        start: start.toISOString(),
        end: end.toISOString()
    });

    const response = await fetch(`${API_BASE}/api/positions/${trackerID}?${params}`);
    if (!response.ok) {
        if (response.status === 404) {
            throw new Error(`Tracker ${trackerID} not found`);
        } else if (response.status === 400) {
            throw new Error('Invalid date format');
        }
        throw new Error(`Failed to fetch positions: ${response.statusText}`);
    }
    const data = await response.json();
    return data.positions || [];
}

// Format a distance in metres for display.
function formatDistance(m) {
    if (m >= 1000) return `${(m / 1000).toFixed(1)} km`;
    return `${Math.round(m)} m`;
}

// Great-circle distance in metres between two lat/lon points (haversine).
// Mirrors haversineDistance() in api.go so the panel's "distance from POI"
// matches server-side maths.
function haversineMeters(lat1, lon1, lat2, lon2) {
    const R = 6371000;
    const toRad = (d) => d * Math.PI / 180;
    const dPhi = toRad(lat2 - lat1);
    const dLambda = toRad(lon2 - lon1);
    const a = Math.sin(dPhi / 2) ** 2
        + Math.cos(toRad(lat1)) * Math.cos(toRad(lat2)) * Math.sin(dLambda / 2) ** 2;
    return R * 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1 - a));
}

// Closest configured POI to a point, with its distance. Null when no POIs.
function nearestPOI(lat, lng) {
    let best = null;
    let bestDist = Infinity;
    for (const p of pois) {
        const d = haversineMeters(lat, lng, p.lat, p.lon);
        if (d < bestDist) { bestDist = d; best = p; }
    }
    return best ? { poi: best, dist: bestDist } : null;
}

// Human-friendly "time since" for the last-seen line.
function relativeTime(date) {
    const secs = Math.floor((Date.now() - date.getTime()) / 1000);
    if (secs < 45) return 'just now';
    const mins = Math.round(secs / 60);
    if (mins < 60) return `${mins} min ago`;
    const hrs = Math.round(mins / 60);
    if (hrs < 24) return `${hrs} h ago`;
    const days = Math.round(hrs / 24);
    return `${days} d ago`;
}

// Battery pill markup: a shell whose inner fill width is the charge %, followed
// by the percentage in mono. Turns amber below 20% (DESIGN.md battery rule).
function batteryPill(pct) {
    if (pct === null || pct === undefined) {
        return '<span class="battery"><span class="batt-pct mono">—</span></span>';
    }
    const clamped = Math.max(0, Math.min(100, pct));
    const low = clamped < 20 ? ' low' : '';
    return `<span class="battery${low}">
        <span class="batt-shell"><span class="batt-fill" style="width:${clamped}%"></span></span>
        <span class="batt-pct mono">${clamped}%</span>
    </span>`;
}

// Fetch per-cat activity summary
async function fetchActivity(days = 4) {
    const response = await fetch(`${API_BASE}/api/activity?days=${days}`);
    if (!response.ok) {
        throw new Error(`Failed to fetch activity: ${response.statusText}`);
    }
    return response.json();
}

// Render one merged card per cat: identity (dot + name) + status (battery,
// last-seen) + activity (7-day distance chart, today/week totals). Draws from
// both the tracker roster and whatever activity/position data has arrived so
// far, so it renders progressively as each source loads (DESIGN.md §1).
function renderCatCards() {
    const container = document.getElementById('cat-cards');
    if (!container) return;

    const roster = [...allTrackers].sort((a, b) => a.id - b.id);
    if (roster.length === 0) {
        container.innerHTML = '';
        return;
    }

    // Index activity payload by tracker id for quick lookup.
    const activityById = {};
    (activityData?.trackers || []).forEach(t => { activityById[t.id] = t; });

    const dayLabel = (date) => new Date(date + 'T00:00:00')
        .toLocaleDateString(undefined, { weekday: 'short' });

    container.innerHTML = roster.map((tracker) => {
        const accent = accentForIndex(trackerPaletteIndex(tracker.id));
        const days = activityById[tracker.id]?.days || [];

        // Status from the latest stored position (newest-first list).
        const positions = positionsByTracker[tracker.id] || [];
        const latest = positions[0];
        const battery = latest ? batteryPill(latest.battery) : batteryPill(null);
        let lastSeen = 'No recent fixes';
        if (latest) {
            lastSeen = `Last seen ${relativeTime(new Date(latest.timestamp))}`;
            const near = nearestPOI(latest.lat, latest.lng);
            if (near) {
                lastSeen += ` · ${formatDistance(near.dist)} from ${near.poi.name}`;
            }
        }

        // Bars: height = share of the cat's own peak day. Last bar (today) uses
        // the full accent; earlier days use the muted accent tint.
        const maxDist = Math.max(1, ...days.map(d => d.distance_m));
        const bars = days.map((d, i) => {
            const isToday = i === days.length - 1;
            const pct = Math.round((d.distance_m / maxDist) * 100);
            const fill = isToday ? accent.color : accent.muted;
            const data = `data-name="${tracker.name}" data-date="${d.date}"`
                + ` data-dist="${d.distance_m}" data-range="${d.range_m}"`
                + ` data-active="${d.active_fraction}" data-fixes="${d.fixes}"`;
            return `<div class="activity-bar ${isToday ? 'today' : ''}" ${data}>
                <div class="activity-bar-fill" style="height:${pct}%;background:${fill}"></div>
            </div>`;
        }).join('');

        const labels = days.map((d, i) => {
            const isToday = i === days.length - 1;
            const text = isToday ? 'Today' : dayLabel(d.date);
            const style = isToday ? ` style="color:${accent.color}"` : '';
            return `<span class="${isToday ? 'today' : ''}"${style}>${text}</span>`;
        }).join('');

        const chart = days.length
            ? `<div class="cat-bars">${bars}</div><div class="cat-daylabels">${labels}</div>`
            : '';

        const today = days[days.length - 1];
        const weekDist = days.reduce((sum, d) => sum + d.distance_m, 0);
        const totals = days.length ? `
            <div class="cat-divider"></div>
            <div class="cat-totals">
                <span><b class="mono">${formatDistance(today ? today.distance_m : 0)}</b> today</span>
                <span><b class="mono">${formatDistance(weekDist)}</b> / wk</span>
            </div>` : '';

        return `
            <div class="cat-card">
                <div class="cat-top">
                    <span class="cat-dot" style="background:${accent.color}"></span>
                    <span class="cat-name">${tracker.name}</span>
                    ${battery}
                </div>
                <div class="cat-meta">${lastSeen}</div>
                ${chart}
                ${totals}
            </div>`;
    }).join('');

    attachActivityTooltip(container);
}

// Single floating popup, shared by all bars, shown on hover. Idempotent: the
// container element persists across re-renders (only its innerHTML changes), so
// the hover listeners are bound at most once.
function attachActivityTooltip(panel) {
    if (panel.dataset.tipBound) return;
    panel.dataset.tipBound = '1';

    let tip = document.getElementById('activity-tooltip');
    if (!tip) {
        tip = document.createElement('div');
        tip.id = 'activity-tooltip';
        document.body.appendChild(tip);
    }

    const fmtDate = (date) => new Date(date + 'T00:00:00')
        .toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short' });

    const show = (bar, x, y) => {
        const d = bar.dataset;
        tip.innerHTML = `
            <div class="tip-head">${d.name} · ${fmtDate(d.date)}</div>
            <div class="tip-row"><span>Distance</span><b>${formatDistance(+d.dist)}</b></div>
            <div class="tip-row"><span>Range</span><b>${formatDistance(+d.range)}</b></div>
            <div class="tip-row"><span>Active</span><b>${Math.round(d.active * 100)}%</b></div>
            <div class="tip-row"><span>Fixes</span><b>${d.fixes}</b></div>
        `;
        tip.classList.add('show');
        // Position above-right of the cursor, clamped to the viewport.
        const r = tip.getBoundingClientRect();
        let left = x + 14;
        let top = y - r.height - 10;
        if (left + r.width > window.innerWidth - 8) left = x - r.width - 14;
        if (top < 8) top = y + 16;
        tip.style.left = `${left}px`;
        tip.style.top = `${top}px`;
    };

    const hide = () => tip.classList.remove('show');

    panel.addEventListener('mousemove', (e) => {
        const bar = e.target.closest('.activity-bar');
        if (bar) show(bar, e.clientX, e.clientY);
        else hide();
    });
    panel.addEventListener('mouseleave', hide);
}

// Load configured POIs (from /api/status) so cards can show the nearest one.
// Swallows errors — without POIs the last-seen line just omits the location.
async function loadPOIs() {
    try {
        const res = await fetch(`${API_BASE}/api/status`);
        if (!res.ok) return;
        const data = await res.json();
        pois = data.pois || [];
        renderCatCards();
    } catch (err) {
        console.error('Error loading POIs:', err);
    }
}

// Load activity data and re-render the cat cards. Swallows errors — the cards
// still render their identity/status half from position data alone.
async function loadActivity() {
    try {
        activityData = await fetchActivity(7);
        renderCatCards();
    } catch (err) {
        console.error('Error loading activity:', err);
    }
}

// Initialize the map
function initMap(center = [59.9139, 10.7522], zoom = DEFAULT_ZOOM) {
    if (map) {
        return;
    }

    map = L.map('map').setView(center, zoom);

    L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
        attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors',
        maxZoom: 19
    }).addTo(map);

    // Re-tune the heat map when the zoom changes, so radius/blur track scale
    // while Auto-tune is on. No-op (bar a cheap re-render) when it's off.
    map.on('zoomend', () => {
        if (heatMapSettings.auto) renderHeatMaps(false);
    });
}

function calculateCenter(positions) {
    if (positions.length === 0) {
        return null;
    }

    let sumLat = 0;
    let sumLng = 0;

    positions.forEach(pos => {
        sumLat += pos.lat;
        sumLng += pos.lng;
    });

    return [sumLat / positions.length, sumLng / positions.length];
}

// Draw the recent-movement trail per tracker: consecutive fixes since 06:00
// joined as age-faded line segments, with an hourly dot at the fix nearest
// each whole hour, layered above the heatmap.
// Reuses the positions already in positionsByTracker — no extra API call.
function renderTrails() {
    Object.values(trailLayers).forEach(group => map.removeLayer(group));
    trailLayers = {};

    // Most recent 06:00 local time (yesterday's if it's currently before 6am).
    const cutoffDate = new Date();
    cutoffDate.setHours(TRAIL_START_HOUR, 0, 0, 0);
    if (cutoffDate.getTime() > Date.now()) cutoffDate.setDate(cutoffDate.getDate() - 1);
    const cutoff = cutoffDate.getTime();
    const now = Date.now();

    sortedTrackerIds().forEach((id) => {
        const positions = positionsByTracker[id] || [];

        // Keep only fixes inside the window, oldest -> newest so segments
        // connect in travel order. (positionsByTracker is newest-first.)
        const recent = positions
            .filter(p => new Date(p.timestamp).getTime() >= cutoff)
            .sort((a, b) => new Date(a.timestamp) - new Date(b.timestamp));
        if (recent.length < 2) return;

        const base = paletteSwatch(paletteForIndex(trackerPaletteIndex(id)));
        const group = L.layerGroup();

        // Coordinates the spline interpolates over (control points).
        const pts = recent.map(p => [p.lat, p.lng]);

        // One curved polyline per original segment, so each keeps its own
        // age-fade color while the corners between fixes are rounded.
        for (let i = 0; i < recent.length - 1; i++) {
            const midTime = (new Date(recent[i].timestamp).getTime() + new Date(recent[i + 1].timestamp).getTime()) / 2;
            L.polyline(splineSegment(pts, i), trailSegmentStyle(base, midTime)).addTo(group);
        }

        // Hourly dots at the fix nearest each whole hour (within tolerance).
        // Dots piling up on one spot = the cat stayed there. They mark real
        // fixes, not spline-interpolated points.
        for (let t = cutoff + 3600000; t <= now; t += 3600000) {
            let best = null;
            let bestDelta = Infinity;
            recent.forEach(p => {
                const delta = Math.abs(new Date(p.timestamp).getTime() - t);
                if (delta < bestDelta) { bestDelta = delta; best = p; }
            });
            if (!best || bestDelta > HOUR_DOT_TOLERANCE_MIN * 60000) continue;

            L.circleMarker([best.lat, best.lng], {
                radius: 4.5,
                color: '#fff',
                weight: 1.5,
                fillColor: base,
                fillOpacity: 0.95
            }).addTo(group).bindTooltip(new Date(t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }));
        }

        group.addTo(map);
        trailLayers[id] = group;
    });
}

// Escape user-supplied text (waypoint names) for safe HTML interpolation.
function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, c => ({
        '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
    }[c]));
}

// Whole days covered by the current date-range selection (the unit the
// waypoint endpoints take).
function currentDays() {
    const r = getDateRange();
    if (!r.start || !r.end || isNaN(r.start) || isNaN(r.end)) return 7;
    return Math.min(365, Math.max(1, Math.ceil((r.end - r.start) / 86400000)));
}

// Quadratic bezier from a to b, bowed to a fixed side so reciprocal edges
// (A→B and B→A) separate visually instead of overlapping.
function curvedEdge(a, b) {
    const mx = (a[0] + b[0]) / 2, my = (a[1] + b[1]) / 2;
    const dx = b[0] - a[0], dy = b[1] - a[1];
    const cx = mx - dy * 0.2, cy = my + dx * 0.2;
    const pts = [];
    for (let s = 0; s <= 20; s++) {
        const t = s / 20, u = 1 - t;
        pts.push([u * u * a[0] + 2 * u * t * cx + t * t * b[0],
                  u * u * a[1] + 2 * u * t * cy + t * t * b[1]]);
    }
    return pts;
}

// Popup content for a waypoint marker: totals, last visit, a 24-hour dwell
// histogram, and a rename button (posts to PUT /api/waypoints/{id}).
function waypointPopupHtml(wp) {
    const hours = wp.hours || [];
    const max = Math.max(1, ...hours);
    const bars = hours.map((v, h) =>
        `<div title="${h}:00 — ${(v / 3600).toFixed(1)} h" `
        + `style="flex:1;height:${Math.max(4, Math.round(v / max * 100))}%;`
        + `background:#16181D;border-radius:1px;opacity:${v > 0 ? 1 : 0.12}"></div>`
    ).join('');
    return `<div class="wp-popup">
        <div class="wp-title">${escapeHtml(wp.name)}</div>
        <div class="wp-row-line"><span>Total</span><b>${(wp.dwell_seconds / 3600).toFixed(1)} h</b></div>
        <div class="wp-row-line"><span>Share</span><b>${Math.round((wp.share || 0) * 100)}%</b></div>
        <div class="wp-row-line"><span>Visits</span><b>${wp.visits}</b></div>
        <div class="wp-row-line"><span>Last visit</span><b>${relativeTime(new Date(wp.last_visit))}</b></div>
        <div class="wp-hist">${bars}</div>
        <div class="wp-hist-labels"><span>0</span><span>6</span><span>12</span><span>18</span><span>24</span></div>
        <button class="wp-rename" onclick="renameWaypoint(${wp.id})">Rename</button>
    </div>`;
}

// Draw one tracker's waypoint markers (sized by dwell share, cat accent color)
// and, when enabled, its transition graph as curved directed edges whose width
// tracks the transition probability.
function renderWaypointLayer(id) {
    if (waypointMarkers[id]) { map.removeLayer(waypointMarkers[id]); delete waypointMarkers[id]; }
    if (routeLayers[id]) { map.removeLayer(routeLayers[id]); delete routeLayers[id]; }
    if (!map) return;

    const wps = waypointsByTracker[id] || [];
    if (wps.length === 0) return;
    const accent = accentForIndex(trackerPaletteIndex(id)).color;

    if (waypointSettings.places) {
        const group = L.layerGroup();
        wps.forEach(wp => {
            L.circleMarker([wp.lat, wp.lon], {
                radius: 6 + 14 * Math.sqrt(wp.share || 0),
                color: '#fff',
                weight: 2,
                fillColor: accent,
                fillOpacity: 0.85
            }).addTo(group).bindPopup(waypointPopupHtml(wp));
        });
        group.addTo(map);
        waypointMarkers[id] = group;
    }

    const edges = transitionsByTracker[id] || [];
    if (waypointSettings.routes && edges.length > 0) {
        const byId = {};
        wps.forEach(wp => { byId[wp.id] = wp; });
        const group = L.layerGroup();
        edges.forEach(e => {
            const a = byId[e.from], b = byId[e.to];
            if (!a || !b) return;
            const pts = curvedEdge([a.lat, a.lon], [b.lat, b.lon]);
            L.polyline(pts, {
                color: accent,
                weight: 1 + 5 * e.probability,
                opacity: 0.75
            }).addTo(group).bindTooltip(
                `${escapeHtml(a.name)} → ${escapeHtml(b.name)} · ${Math.round(e.probability * 100)}% (${e.count}×)`
            );
            // Small dot near the target end to show direction.
            const tip = pts[Math.floor(pts.length * 0.8)];
            L.circleMarker(tip, {
                radius: 2.5, weight: 0, fillColor: accent, fillOpacity: 1
            }).addTo(group);
        });
        group.addTo(map);
        routeLayers[id] = group;
    }
}

function refreshWaypointLayers() {
    allTrackers.forEach(t => renderWaypointLayer(t.id));
}

// Sidebar "Top places" panel: per cat, the waypoints ranked by dwell share,
// with the share change vs. the previous period as a trend arrow. Clicking a
// row pans the map to the place.
function renderTopPlaces() {
    const container = document.getElementById('top-places');
    if (!container) return;

    const roster = [...allTrackers].sort((a, b) => a.id - b.id);
    const sections = roster.map(t => {
        const wps = (waypointsByTracker[t.id] || [])
            .slice()
            .sort((a, b) => b.share - a.share)
            .slice(0, 5);
        if (wps.length === 0) return '';
        const accent = accentForIndex(trackerPaletteIndex(t.id)).color;
        const rows = wps.map(wp => {
            const delta = (wp.share || 0) - (wp.prev_share || 0);
            let trend = '<span class="place-trend trend-flat">–</span>';
            if (delta > 0.02) {
                trend = `<span class="place-trend trend-up">▲${Math.round(delta * 100)}</span>`;
            } else if (delta < -0.02) {
                trend = `<span class="place-trend trend-down">▼${Math.round(-delta * 100)}</span>`;
            }
            return `<div class="place-row" data-lat="${wp.lat}" data-lon="${wp.lon}">
                <span class="place-dot" style="background:${accent}"></span>
                <span class="place-name">${escapeHtml(wp.name)}</span>
                <span class="place-share mono">${Math.round((wp.share || 0) * 100)}%</span>
                ${trend}
            </div>`;
        }).join('');
        return `<div class="place-card">
            <div class="place-cat" style="color:${accent}">${escapeHtml(t.name)}</div>${rows}
        </div>`;
    }).filter(Boolean).join('');

    container.innerHTML = sections
        ? `<div><div class="section-label">Top places · ${currentDays()}d</div></div>${sections}`
        : '';

    container.querySelectorAll('.place-row').forEach(row => {
        row.addEventListener('click', () => {
            if (map) map.setView([+row.dataset.lat, +row.dataset.lon], 18);
        });
    });
}

// Fetch waypoints and transitions for every tracker and refresh the layers
// and panel. Swallows per-tracker errors — a cat without data just gets no
// waypoints.
async function loadWaypoints() {
    if (allTrackers.length === 0) return;
    const days = currentDays();

    await Promise.all(allTrackers.map(async t => {
        try {
            const [wpRes, trRes] = await Promise.all([
                fetch(`${API_BASE}/api/waypoints?tracker_id=${t.id}&days=${days}`)
                    .then(r => r.ok ? r.json() : { waypoints: [] }),
                fetch(`${API_BASE}/api/waypoints/transitions?tracker_id=${t.id}&days=${days}`)
                    .then(r => r.ok ? r.json() : { edges: [] })
            ]);
            waypointsByTracker[t.id] = wpRes.waypoints || [];
            transitionsByTracker[t.id] = trRes.edges || [];
            renderWaypointLayer(t.id);
        } catch (err) {
            console.error(`Failed to load waypoints for tracker ${t.id}:`, err);
        }
    }));

    renderTopPlaces();
}

// Prompt for a waypoint name and persist it. Empty input clears the name back
// to the auto-label. Invoked from the popup's Rename button.
async function renameWaypoint(id) {
    let current = '';
    for (const wps of Object.values(waypointsByTracker)) {
        const wp = wps.find(w => w.id === id);
        if (wp) { current = wp.named ? wp.name : ''; break; }
    }

    const name = prompt('Name this place (leave empty to reset to the auto-label):', current);
    if (name === null) return;

    try {
        const res = await fetch(`${API_BASE}/api/waypoints/${id}`, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ name: name.trim() })
        });
        if (!res.ok) throw new Error(`Rename failed: ${res.statusText}`);
        if (map) map.closePopup();
        loadWaypoints();
    } catch (err) {
        console.error('Error renaming waypoint:', err);
        showError(err.message || 'Rename failed');
    }
}

// ----- Home range: KDE 50% core / 95% home-range contours (issue #3) -----

// Convert API rings ([lon,lat] pairs, closed) to Leaflet latlng arrays.
function ringsToLatLngs(rings) {
    return (rings || []).map(ring => ring.map(([lon, lat]) => [lat, lon]));
}

// Draw one tracker's contour pair: 95% home range as a thin outline with a
// faint fill, 50% core as a stronger fill in the cat's accent color. With two
// cats enabled the translucent home fills blend where they overlap, which
// reads as the shared territory — no polygon clipping needed.
function renderRangeLayer(id) {
    if (rangeLayers[id]) { map.removeLayer(rangeLayers[id]); delete rangeLayers[id]; }
    if (!map || !rangeSettings.enabled || rangeSettings.cats[id] === false) return;

    const data = rangeByTracker[id];
    if (!data) return;
    const accent = accentForIndex(trackerPaletteIndex(id)).color;

    const group = L.layerGroup();
    const home = ringsToLatLngs(data.home);
    const core = ringsToLatLngs(data.core);
    home.forEach(latlngs => {
        L.polygon(latlngs, {
            color: accent, weight: 2, opacity: 0.9,
            fillColor: accent, fillOpacity: 0.07
        }).addTo(group);
    });
    core.forEach(latlngs => {
        L.polygon(latlngs, {
            color: accent, weight: 1, opacity: 0.8,
            fillColor: accent, fillOpacity: 0.35
        }).addTo(group);
    });

    if (group.getLayers().length === 0) return;
    const tracker = allTrackers.find(t => t.id === id);
    group.bindTooltip(
        `${tracker ? escapeHtml(tracker.name) : 'Tracker ' + id} · ${data.month || '30d'} · `
        + `${data.fixes_used} fixes · bandwidth ${Math.round(data.bandwidth_m)} m`
    );
    group.addTo(map);
    rangeLayers[id] = group;
}

function refreshRangeLayers() {
    allTrackers.forEach(t => renderRangeLayer(t.id));
}

// Fetch the month's contours for every tracker and refresh the layers.
// Swallows per-tracker errors — a cat without data just gets no contours.
async function loadRanges() {
    if (allTrackers.length === 0 || !rangeSettings.month) return;

    await Promise.all(allTrackers.map(async t => {
        try {
            const res = await fetch(
                `${API_BASE}/api/range?tracker_id=${t.id}&month=${rangeSettings.month}`);
            rangeByTracker[t.id] = res.ok ? await res.json() : null;
        } catch (err) {
            console.error(`Failed to load range for tracker ${t.id}:`, err);
            rangeByTracker[t.id] = null;
        }
        renderRangeLayer(t.id);
    }));
}

// Per-cat toggle rows inside the range card, built once the roster is known.
function renderRangeCatToggles() {
    const container = document.getElementById('range-cats');
    if (!container) return;

    container.innerHTML = '';
    [...allTrackers].sort((a, b) => a.id - b.id).forEach(t => {
        const accent = accentForIndex(trackerPaletteIndex(t.id)).color;
        const on = rangeSettings.cats[t.id] !== false;

        const row = document.createElement('div');
        row.className = 'range-cat-row';
        row.innerHTML = `
            <span class="cat-dot" style="background:${accent}"></span>
            <span class="range-cat-name">${escapeHtml(t.name)}</span>
            <button class="toggle${on ? ' on' : ''}" role="switch" aria-checked="${on}"
                    aria-label="Show ${escapeHtml(t.name)}'s range contours">
                <span class="knob"></span>
            </button>`;
        row.querySelector('.toggle').addEventListener('click', (e) => {
            const btn = e.currentTarget;
            const nowOn = rangeSettings.cats[t.id] === false;
            rangeSettings.cats[t.id] = nowOn;
            btn.classList.toggle('on', nowOn);
            btn.setAttribute('aria-checked', String(nowOn));
            renderRangeLayer(t.id);
        });
        container.appendChild(row);
    });
}

// Fetch the monthly cat-vs-cat overlap series and draw the chart.
async function loadOverlap() {
    try {
        const res = await fetch(`${API_BASE}/api/range/overlap?months=12`);
        overlapSeries = res.ok ? await res.json() : [];
    } catch (err) {
        console.error('Failed to load overlap series:', err);
        overlapSeries = [];
    }
    drawOverlapChart();
}

// Bar chart of the monthly overlap index (0-1). The selected month is inked;
// clicking a bar steps the range layer to that month.
function drawOverlapChart() {
    const canvas = document.getElementById('overlap-chart');
    if (!canvas) return;
    const empty = document.getElementById('overlap-empty');

    const dpr = window.devicePixelRatio || 1;
    const w = canvas.clientWidth, h = canvas.clientHeight;
    canvas.width = w * dpr;
    canvas.height = h * dpr;
    const ctx = canvas.getContext('2d');
    ctx.scale(dpr, dpr);
    ctx.clearRect(0, 0, w, h);

    const points = overlapSeries || [];
    if (empty) empty.style.display = points.some(p => p.index !== null) ? 'none' : 'block';
    if (points.length === 0 || w === 0) return;

    const gap = 3;
    const barW = Math.max(2, (w - gap * (points.length - 1)) / points.length);
    const baseline = h - 14; // room for month labels

    points.forEach((p, i) => {
        const x = i * (barW + gap);
        const selected = p.month === rangeSettings.month;
        if (p.index === null || p.index === undefined) {
            // No usable month: tick mark at the baseline.
            ctx.fillStyle = '#DFE3E7';
            ctx.fillRect(x, baseline - 2, barW, 2);
        } else {
            const bh = Math.max(2, p.index * (baseline - 6));
            ctx.fillStyle = selected ? '#16181D' : '#B4BAC2';
            if (ctx.roundRect) {
                ctx.beginPath();
                ctx.roundRect(x, baseline - bh, barW, bh, 2);
                ctx.fill();
            } else {
                ctx.fillRect(x, baseline - bh, barW, bh);
            }
        }
        if (i % 3 === 0 || selected) {
            ctx.fillStyle = selected ? '#16181D' : '#9198A2';
            ctx.font = '9px "JetBrains Mono", monospace';
            ctx.textAlign = 'center';
            ctx.fillText(p.month.slice(2), x + barW / 2, h - 3);
        }
    });
}

// ----- Presence: WiFi-zone at-home timeline + excursion stats (issue #4) -----

const PRESENCE_DAYS = 7;
const EXCURSION_DAYS = 30;

// Semantic state colors. Ribbons are zone-only (home/away) — the WiFi zone is
// the single source of truth; the SureHub flap state is known-stale and never
// feeds presence state (see presence.go).
const PRESENCE_STATE_COLORS = { home: '#17BE7E', away: '#E5484D' };

// "3h 12m"-style formatting of a duration in seconds (presence summaries).
function formatDurationS(sec) {
    const mins = Math.round(sec / 60);
    if (mins < 60) return `${mins}m`;
    const hours = Math.floor(mins / 60);
    if (hours < 24) return mins % 60 ? `${hours}h ${mins % 60}m` : `${hours}h`;
    return `${Math.floor(hours / 24)}d ${hours % 24}h`;
}

// Upper-edge label for an excursion duration bucket ("5m", "2h").
function excBucketLabel(edgeS) {
    if (edgeS < 3600) return `${Math.round(edgeS / 60)}m`;
    return `${Math.round(edgeS / 3600)}h`;
}

// Fetch the presence timeline and excursion stats for every tracker and
// re-render both panels. Swallows per-tracker errors — a cat without zone
// data just gets no ribbons.
async function loadPresence() {
    if (allTrackers.length === 0) return;

    await Promise.all(allTrackers.map(async t => {
        try {
            const [pRes, eRes] = await Promise.all([
                fetch(`${API_BASE}/api/presence?tracker_id=${t.id}&days=${PRESENCE_DAYS}`)
                    .then(r => r.ok ? r.json() : null),
                fetch(`${API_BASE}/api/presence/excursions?tracker_id=${t.id}&days=${EXCURSION_DAYS}`)
                    .then(r => r.ok ? r.json() : null)
            ]);
            presenceByTracker[t.id] = pRes;
            excursionsByTracker[t.id] = eRes ? eRes.stats : null;
        } catch (err) {
            console.error(`Failed to load presence for tracker ${t.id}:`, err);
        }
    }));

    renderPresenceTimelines();
    renderExcursionStats();
}

// One 24h ribbon per day stacked oldest→newest (today last), per cat. The
// server clips intervals to the day, so segments always land inside the bar;
// the bar's track color shows through where no zone data exists.
function renderPresenceTimelines() {
    const container = document.getElementById('presence-timelines');
    if (!container) return;

    const roster = [...allTrackers].sort((a, b) => a.id - b.id);
    const timeOpt = { hour: '2-digit', minute: '2-digit' };

    const sections = roster.map(t => {
        const timeline = presenceByTracker[t.id]?.timeline || [];
        if (timeline.length === 0) return '';
        const accent = accentForIndex(trackerPaletteIndex(t.id)).color;

        const rows = timeline.map((d, i) => {
            const isToday = i === timeline.length - 1;
            const dayStart = new Date(d.date + 'T00:00:00').getTime();
            const segs = d.intervals.map(iv => {
                const s = new Date(iv.start).getTime();
                const e = new Date(iv.end).getTime();
                const left = Math.max(0, (s - dayStart) / 864000);
                const width = Math.max(0.4, (e - s) / 864000);
                const color = PRESENCE_STATE_COLORS[iv.state] || '#B4BAC2';
                const tip = `${iv.state} ${new Date(iv.start).toLocaleTimeString([], timeOpt)}`
                    + ` – ${new Date(iv.end).toLocaleTimeString([], timeOpt)}`;
                return `<div class="presence-seg" style="left:${left}%;width:${width}%;background:${color}" title="${tip}"></div>`;
            }).join('');
            const label = isToday ? 'Today'
                : new Date(d.date + 'T00:00:00').toLocaleDateString(undefined, { weekday: 'short' });
            return `<div class="presence-row${isToday ? ' today' : ''}">
                <span class="presence-day">${label}</span>
                <div class="presence-bar">${segs}</div>
            </div>`;
        }).join('');

        const today = timeline[timeline.length - 1];
        const summary = `home ${formatDurationS(today.time_home_s)} · ${today.excursions} excursion${today.excursions === 1 ? '' : 's'}`;

        return `<div class="presence-cat">
            <div class="presence-cat-head">
                <span class="cat-dot" style="background:${accent}"></span>
                <span class="presence-cat-name">${escapeHtml(t.name)}</span>
                <span class="presence-today mono">${escapeHtml(summary)}</span>
            </div>
            ${rows}
            <div class="presence-ticks"><span>0</span><span>6</span><span>12</span><span>18</span><span>24</span></div>
        </div>`;
    }).filter(Boolean).join('');

    container.innerHTML = sections || '<div class="panel-empty">No WiFi-zone data yet.</div>';
}

// Excursion stats panel: per cat, a duration histogram (fixed buckets from
// the API) and a departure-hour histogram — the daily rhythm at a glance.
function renderExcursionStats() {
    const container = document.getElementById('excursion-charts');
    if (!container) return;

    const roster = [...allTrackers].sort((a, b) => a.id - b.id);

    const bars = (counts, accent, title) => {
        const max = Math.max(1, ...counts);
        return counts.map((c, i) =>
            `<div class="exc-bar" title="${title(i)}: ${c}">
                <div class="exc-bar-fill" style="height:${Math.max(5, c / max * 100)}%;background:${accent};opacity:${c > 0 ? 1 : 0.15}"></div>
            </div>`
        ).join('');
    };

    const sections = roster.map(t => {
        const stats = excursionsByTracker[t.id];
        if (!stats) return '';
        const accent = accentForIndex(trackerPaletteIndex(t.id)).color;
        const n = (stats.excursions || []).length;
        if (n === 0) {
            return `<div class="exc-head">
                <span class="cat-dot" style="background:${accent}"></span>
                <span class="exc-cat-name">${escapeHtml(t.name)}</span>
                <span class="exc-summary">no excursions</span>
            </div>`;
        }

        const longest = Math.max(...stats.excursions.map(e => e.duration_s));
        const edges = stats.duration_bucket_s || [];
        const bucketName = (i) => i === 0 ? `<${excBucketLabel(edges[0])}`
            : i >= edges.length ? `${excBucketLabel(edges[edges.length - 1])}+`
            : `≤${excBucketLabel(edges[i])}`;
        const durLabels = stats.duration_counts.map((_, i) => `<span>${bucketName(i)}</span>`).join('');

        return `<div class="exc-head">
                <span class="cat-dot" style="background:${accent}"></span>
                <span class="exc-cat-name">${escapeHtml(t.name)}</span>
                <span class="exc-summary mono">${n} out · longest ${formatDurationS(longest)}</span>
            </div>
            <div class="exc-sub">Duration</div>
            <div class="exc-bars">${bars(stats.duration_counts, accent, bucketName)}</div>
            <div class="exc-labels">${durLabels}</div>
            <div class="exc-sub">Departure</div>
            <div class="exc-bars">${bars(stats.departure_hours, accent, h => `${h}:00`)}</div>
            <div class="exc-labels"><span>0</span><span>6</span><span>12</span><span>18</span><span>24</span></div>`;
    }).filter(Boolean).join('');

    container.innerHTML = sections || '<div class="panel-empty">No excursions recorded.</div>';
}

// ----- Behavior: HMM rest/forage/transit segmentation (issue #5) -----

// Semantic state colors, shared by the track layer, budget bars, hour matrix,
// and the card legend. Order matters: budget columns stack rest at the
// bottom, transit on top.
const BEHAVIOR_STATES = ['rest', 'forage', 'transit'];
const BEHAVIOR_STATE_COLORS = { rest: '#3B82F6', forage: '#F59E0B', transit: '#EF4444' };
const BUDGET_DAYS = 7;
const BEHAVIOR_MATRIX_DAYS = 30;

// Fetch decoded segments (current date range), the daily budget, and the
// hour matrix for every tracker, then re-render the layer and both panels.
// Swallows per-tracker errors — a cat without a fitted model (too little
// data) just gets no behavior visualizations.
async function loadBehavior() {
    if (allTrackers.length === 0) return;
    const range = getDateRange();
    const win = new URLSearchParams({ from: range.start.toISOString(), to: range.end.toISOString() });

    await Promise.all(allTrackers.map(async t => {
        try {
            const [segRes, budRes, matRes] = await Promise.all([
                fetch(`${API_BASE}/api/behavior/segments?tracker_id=${t.id}&${win}`)
                    .then(r => r.ok ? r.json() : null),
                fetch(`${API_BASE}/api/behavior/budget?tracker_id=${t.id}&days=${BUDGET_DAYS}`)
                    .then(r => r.ok ? r.json() : null),
                fetch(`${API_BASE}/api/behavior/matrix?tracker_id=${t.id}&days=${BEHAVIOR_MATRIX_DAYS}`)
                    .then(r => r.ok ? r.json() : null)
            ]);
            behaviorSegByTracker[t.id] = segRes;
            behaviorBudgetByTracker[t.id] = budRes;
            behaviorMatrixByTracker[t.id] = matRes;
            renderBehaviorLayer(t.id);
        } catch (err) {
            console.error(`Failed to load behavior for tracker ${t.id}:`, err);
        }
    }));

    renderBehaviorBudget();
    renderBehaviorMatrix();
}

// Draw one tracker's decoded steps as straight segments colored by state.
// Straight lines, not splines: each step has its own state, so the segment
// boundaries must stay exactly on the decoded steps.
function renderBehaviorLayer(id) {
    if (behaviorLayers[id]) { map.removeLayer(behaviorLayers[id]); delete behaviorLayers[id]; }
    if (!map || !behaviorSettings.tracks) return;

    const steps = behaviorSegByTracker[id]?.steps || [];
    if (steps.length === 0) return;

    const group = L.layerGroup();
    steps.forEach(s => {
        L.polyline([s.from, s.to], {
            color: BEHAVIOR_STATE_COLORS[s.state] || '#9198A2',
            weight: 3.5,
            opacity: 0.9,
            lineCap: 'round'
        }).addTo(group).bindTooltip(
            `${s.state} · ${formatDistance(s.step_m)} · ${new Date(s.t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}`
        );
    });
    group.addTo(map);
    behaviorLayers[id] = group;
}

function refreshBehaviorLayers() {
    allTrackers.forEach(t => renderBehaviorLayer(t.id));
}

// "Activity budget" panel: per cat, one full-height stacked column per day —
// the share of decoded time in each state (rest bottom, transit top). The
// hover title carries absolute hours; empty track means no decoded steps that
// day (the cat was home on WiFi-zone fixes, or the collar was off).
function renderBehaviorBudget() {
    const container = document.getElementById('behavior-budget');
    if (!container) return;

    const roster = [...allTrackers].sort((a, b) => a.id - b.id);
    const dayLabel = (date) => new Date(date + 'T00:00:00')
        .toLocaleDateString(undefined, { weekday: 'short' });

    const sections = roster.map(t => {
        const days = behaviorBudgetByTracker[t.id]?.budget || [];
        if (days.length === 0) return '';
        const accent = accentForIndex(trackerPaletteIndex(t.id)).color;

        const cols = days.map((d, i) => {
            const total = d.rest_s + d.forage_s + d.transit_s;
            const title = total > 0
                ? `${d.date} — rest ${formatDurationS(d.rest_s)} · forage ${formatDurationS(d.forage_s)} · transit ${formatDurationS(d.transit_s)}`
                : `${d.date} — no decoded steps`;
            const segs = total > 0
                ? BEHAVIOR_STATES.map(s =>
                    `<div style="height:${d[s + '_s'] / total * 100}%;background:${BEHAVIOR_STATE_COLORS[s]}"></div>`
                ).join('')
                : '';
            return `<div class="bh-col" title="${title}">${segs}</div>`;
        }).join('');

        const labels = days.map((d, i) => {
            const isToday = i === days.length - 1;
            return `<span class="${isToday ? 'today' : ''}">${isToday ? 'Today' : dayLabel(d.date)}</span>`;
        }).join('');

        return `<div class="bh-cat">
            <div class="bh-cat-head">
                <span class="cat-dot" style="background:${accent}"></span>
                <span class="bh-cat-name">${escapeHtml(t.name)}</span>
            </div>
            <div class="bh-cols">${cols}</div>
            <div class="bh-collabels">${labels}</div>
        </div>`;
    }).filter(Boolean).join('');

    container.innerHTML = sections
        ? `<div class="bh-sub">Activity budget · ${BUDGET_DAYS}d</div>${sections}`
        : '<div class="panel-empty">No behavior model yet — needs more GPS history.</div>';
}

// "State by hour" panel: per cat, 24 stacked micro-bars (one per hour of
// day) showing the state share within that hour over the last 30 days — the
// crepuscular rhythm view.
function renderBehaviorMatrix() {
    const container = document.getElementById('behavior-matrix');
    if (!container) return;

    const roster = [...allTrackers].sort((a, b) => a.id - b.id);

    const sections = roster.map(t => {
        const resp = behaviorMatrixByTracker[t.id];
        if (!resp || !resp.model_ready) return '';
        const matrix = resp.matrix || [];
        const accent = accentForIndex(trackerPaletteIndex(t.id)).color;

        const rows = matrix.map((shares, h) => {
            const total = (shares || []).reduce((a, b) => a + b, 0);
            const segs = total > 0
                ? BEHAVIOR_STATES.map((s, i) =>
                    `<div style="width:${shares[i] * 100}%;background:${BEHAVIOR_STATE_COLORS[s]}" title="${h}:00 — ${s} ${Math.round(shares[i] * 100)}%"></div>`
                ).join('')
                : '';
            const label = h % 6 === 0 ? String(h) : '';
            return `<div class="bh-hrow"><span class="bh-hr mono">${label}</span><div class="bh-hbar">${segs}</div></div>`;
        }).join('');

        return `<div class="bh-cat">
            <div class="bh-cat-head">
                <span class="cat-dot" style="background:${accent}"></span>
                <span class="bh-cat-name">${escapeHtml(t.name)}</span>
            </div>
            <div class="bh-matrix">${rows}</div>
        </div>`;
    }).filter(Boolean).join('');

    container.innerHTML = sections
        ? `<div class="bh-sub">State by hour · ${BEHAVIOR_MATRIX_DAYS}d</div>${sections}`
        : '';
}

// Derive heat-map settings from the current map zoom and point density, used// when "Auto-tune" is on. Radius/blur are pixel values, so they track zoom;
// intensity per point falls as the total point count rises, so dense ranges
// don't wash out to solid colour. Mutates heatMapSettings and syncs the (view-
// only) slider UI so the numbers stay honest.
function applyAutoTune() {
    const zoom = map ? map.getZoom() : 13;
    const radius = Math.round(Math.max(6, Math.min(22, (zoom - 7) * 2)));
    const blur = Math.round(radius * 0.6);
    const total = Object.values(positionsByTracker).reduce((n, p) => n + p.length, 0);
    const intensity = Math.max(0.08, Math.min(0.5, 24 / Math.sqrt(Math.max(1, total))));

    heatMapSettings.radius = radius;
    heatMapSettings.blur = blur;
    heatMapSettings.intensity = intensity;
    syncSliderUI();
}

// Reflect heatMapSettings into the slider inputs and their value labels.
function syncSliderUI() {
    const set = (slider, value, label, text) => {
        const s = document.getElementById(slider);
        const l = document.getElementById(label);
        if (s) s.value = value;
        if (l) l.textContent = text;
    };
    set('intensity-slider', heatMapSettings.intensity, 'intensity-value', heatMapSettings.intensity.toFixed(2));
    set('radius-slider', heatMapSettings.radius, 'radius-value', heatMapSettings.radius);
    set('blur-slider', heatMapSettings.blur, 'blur-value', heatMapSettings.blur);
}

// Enable/disable the slider group to match the Auto-tune toggle. When auto is
// on the group is dimmed and inert; the sliders only drive the map by hand.
function applyAutoUIState() {
    const stack = document.getElementById('slider-stack');
    if (stack) stack.classList.toggle('auto', heatMapSettings.auto);
    ['intensity-slider', 'radius-slider', 'blur-slider'].forEach(id => {
        const s = document.getElementById(id);
        if (s) s.disabled = heatMapSettings.auto;
    });
}

// Render one heat layer per tracker, each with its assigned palette
function renderHeatMaps(resetView = true) {
    if (heatMapSettings.auto) applyAutoTune();

    Object.values(heatLayers).forEach(layer => map.removeLayer(layer));
    heatLayers = {};
    Object.values(latestMarkers).forEach(marker => map.removeLayer(marker));
    latestMarkers = {};

    const ids = sortedTrackerIds();
    let allPositions = [];

    ids.forEach((id) => {
        const positions = positionsByTracker[id] || [];
        if (positions.length === 0) return;

        const palette = paletteForIndex(trackerPaletteIndex(id));
        const heatData = positions.map(pos => [pos.lat, pos.lng, heatMapSettings.intensity]);

        heatLayers[id] = L.heatLayer(heatData, {
            radius: heatMapSettings.radius,
            blur: heatMapSettings.blur,
            maxZoom: 17,
            max: 3.0,
            gradient: palette
        }).addTo(map);

        const tracker = allTrackers.find(t => t.id === id);
        const latest = positions[0];
        const latestDate = new Date(latest.timestamp);
        const swatch = paletteSwatch(palette);
        latestMarkers[id] = L.marker([latest.lat, latest.lng], { icon: makePinIcon(swatch) })
            .addTo(map)
            .bindPopup(`
                <strong>${tracker ? tracker.name : 'Tracker ' + id}</strong><br>
                Time: ${latestDate.toLocaleString()}<br>
                Battery: ${latest.battery}%
            `);

        allPositions = allPositions.concat(positions);
    });

    renderTrails();

    if (resetView && allPositions.length > 0) {
        const center = calculateCenter(allPositions);
        if (center) {
            map.setView(center, DEFAULT_ZOOM);
        }
    }

    if (allPositions.length === 0) {
        showError('No positions found for any tracker in the selected date range');
    }
}

// Load and display data for all trackers in parallel
async function loadAllTrackerData() {
    if (allTrackers.length === 0) {
        return;
    }

    try {
        setLoading(true);

        const range = getDateRange();
        console.log(`Loading positions for ${allTrackers.length} tracker(s) from ${range.start} to ${range.end}`);

        const results = await Promise.all(
            allTrackers.map(t =>
                fetchPositions(t.id, range.start, range.end)
                    .then(positions => ({ id: t.id, positions }))
                    .catch(err => {
                        console.error(`Failed to load tracker ${t.id}:`, err);
                        return { id: t.id, positions: [] };
                    })
            )
        );

        positionsByTracker = {};
        results.forEach(({ id, positions }) => {
            positionsByTracker[id] = positions;
        });

        const isFirstLoad = !map;
        if (isFirstLoad) {
            initMap();
        }

        renderHeatMaps(isFirstLoad);
        renderCatCards();
        updateMeta();
        loadWaypoints();
        loadBehavior();

        setLoading(false);
        applyAutoUIState();
    } catch (error) {
        console.error('Error loading tracker data:', error);
        setLoading(false);
        showError(error.message);
    }
}

// Trigger an upstream resync with Weenect, then reload the map with the fresh
// data. The POST is synchronous server-side, so it resolves only once the sync
// has finished (or 409 if one was already running).
async function triggerSync(btn) {
    if (btn.disabled) return;

    const label = btn.querySelector('.sync-label');
    btn.disabled = true;
    btn.classList.add('syncing');
    if (label) label.textContent = 'Syncing…';

    try {
        const res = await fetch(`${API_BASE}/api/sync`, { method: 'POST' });
        if (res.status === 409) {
            showError('A sync is already in progress. Try again shortly.');
        } else if (!res.ok) {
            throw new Error(`Sync failed: ${res.statusText}`);
        } else {
            // Pull the freshly-synced positions into the map and panels.
            await loadAllTrackerData();
            loadActivity();
        }
    } catch (error) {
        console.error('Error triggering sync:', error);
        showError(error.message || 'Sync failed');
    } finally {
        btn.classList.remove('syncing');
        if (label) label.textContent = 'Update';
        btn.disabled = false;
    }
}

// Setup event listeners
function setupEventListeners() {
    // Manual resync button
    const syncBtn = document.getElementById('sync-btn');
    if (syncBtn) {
        syncBtn.addEventListener('click', () => triggerSync(syncBtn));
    }

    // Date range segmented control
    document.querySelectorAll('#date-presets .seg').forEach(btn => {
        btn.addEventListener('click', () => {
            document.querySelectorAll('#date-presets .seg').forEach(b => b.classList.remove('active'));
            btn.classList.add('active');

            const days = btn.dataset.days;

            if (days === 'custom') {
                document.getElementById('custom-dates').style.display = 'flex';
                currentDateRange = { custom: true };
            } else {
                document.getElementById('custom-dates').style.display = 'none';
                currentDateRange = { days: parseInt(days) };
                loadAllTrackerData();
            }
        });
    });

    document.getElementById('start-date').addEventListener('change', () => {
        if (currentDateRange.custom) {
            loadAllTrackerData();
        }
    });

    document.getElementById('end-date').addEventListener('change', () => {
        if (currentDateRange.custom) {
            loadAllTrackerData();
        }
    });

    // Auto-tune toggle: when on, settings derive from zoom/density and the
    // slider group is inert; when off, the sliders drive the heat map by hand.
    const toggle = document.getElementById('autotune-toggle');
    if (toggle) {
        toggle.addEventListener('click', () => {
            heatMapSettings.auto = !heatMapSettings.auto;
            toggle.classList.toggle('on', heatMapSettings.auto);
            toggle.setAttribute('aria-checked', String(heatMapSettings.auto));
            applyAutoUIState();
            renderHeatMaps(false);
        });
    }

    // Waypoint layer toggles: re-render from cached data, no refetch.
    const placesToggle = document.getElementById('places-toggle');
    if (placesToggle) {
        placesToggle.addEventListener('click', () => {
            waypointSettings.places = !waypointSettings.places;
            placesToggle.classList.toggle('on', waypointSettings.places);
            placesToggle.setAttribute('aria-checked', String(waypointSettings.places));
            refreshWaypointLayers();
        });
    }

    const routesToggle = document.getElementById('routes-toggle');
    if (routesToggle) {
        routesToggle.addEventListener('click', () => {
            waypointSettings.routes = !waypointSettings.routes;
            routesToggle.classList.toggle('on', waypointSettings.routes);
            routesToggle.setAttribute('aria-checked', String(waypointSettings.routes));
            refreshWaypointLayers();
        });
    }

    // Home-range layer toggle: re-render from cached contours, no refetch.
    const rangeToggle = document.getElementById('range-toggle');
    if (rangeToggle) {
        rangeToggle.addEventListener('click', () => {
            rangeSettings.enabled = !rangeSettings.enabled;
            rangeToggle.classList.toggle('on', rangeSettings.enabled);
            rangeToggle.setAttribute('aria-checked', String(rangeSettings.enabled));
            refreshRangeLayers();
        });
    }

    // Behavior track-coloring toggle: re-render from cached segments, no refetch.
    const behaviorToggle = document.getElementById('behavior-toggle');
    if (behaviorToggle) {
        behaviorToggle.addEventListener('click', () => {
            behaviorSettings.tracks = !behaviorSettings.tracks;
            behaviorToggle.classList.toggle('on', behaviorSettings.tracks);
            behaviorToggle.setAttribute('aria-checked', String(behaviorSettings.tracks));
            refreshBehaviorLayers();
        });
    }

    // Month picker: step the contour layer (and chart highlight) through
    // monthly windows.
    const monthInput = document.getElementById('range-month');
    if (monthInput) {
        monthInput.addEventListener('change', () => {
            if (!monthInput.value) return;
            rangeSettings.month = monthInput.value;
            loadRanges();
            drawOverlapChart();
        });
    }

    // Click an overlap bar to step the range layer to that month.
    const overlapChart = document.getElementById('overlap-chart');
    if (overlapChart) {
        overlapChart.addEventListener('click', (e) => {
            if (overlapSeries.length === 0) return;
            const rect = overlapChart.getBoundingClientRect();
            const i = Math.floor((e.clientX - rect.left) / rect.width * overlapSeries.length);
            const p = overlapSeries[Math.min(Math.max(i, 0), overlapSeries.length - 1)];
            if (!p) return;
            rangeSettings.month = p.month;
            if (monthInput) monthInput.value = p.month;
            loadRanges();
            drawOverlapChart();
        });
    }

    // Heat map setting sliders — re-render with cached positions, no refetch.
    // Only active while Auto-tune is off (the group is inert otherwise).
    document.getElementById('intensity-slider').addEventListener('input', (e) => {
        heatMapSettings.intensity = parseFloat(e.target.value);
        document.getElementById('intensity-value').textContent = heatMapSettings.intensity.toFixed(2);
        renderHeatMaps(false);
    });

    document.getElementById('radius-slider').addEventListener('input', (e) => {
        heatMapSettings.radius = parseInt(e.target.value);
        document.getElementById('radius-value').textContent = heatMapSettings.radius;
        renderHeatMaps(false);
    });

    document.getElementById('blur-slider').addEventListener('input', (e) => {
        heatMapSettings.blur = parseInt(e.target.value);
        document.getElementById('blur-value').textContent = heatMapSettings.blur;
        renderHeatMaps(false);
    });
}

// Subscribe to the server's SSE stream so the map refreshes whenever a sync
// lands (same "Live" behaviour as the radar page). The browser auto-reconnects
// on error; we ignore the initial "connected" event since we just loaded.
function initLiveUpdates() {
    try {
        const es = new EventSource(`${API_BASE}/api/events`);
        es.addEventListener('update', (e) => {
            if (e.data === 'connected') return;
            loadAllTrackerData();
            loadActivity();
            loadWaypoints();
            loadRanges();
            loadOverlap();
            loadPresence();
            loadBehavior();
        });
    } catch (err) {
        console.error('Live updates unavailable:', err);
    }
}

// Main initialization function
async function init() {
    try {
        setLoading(true);

        initializeDateInputs();
        setupEventListeners();
        applyAutoUIState();

        allTrackers = await fetchTrackers();

        if (!allTrackers || allTrackers.length === 0) {
            throw new Error('No trackers found');
        }

        console.log(`Found ${allTrackers.length} tracker(s)`);

        // Default the range month picker to the current (local) month.
        const now = new Date();
        rangeSettings.month = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}`;
        const monthInput = document.getElementById('range-month');
        if (monthInput) monthInput.value = rangeSettings.month;

        renderCatCards();   // identity/status skeleton before data arrives
        renderRangeCatToggles();
        loadActivity();
        loadPOIs();
        loadOverlap();
        loadPresence();

        await loadAllTrackerData();
        loadRanges();
        initLiveUpdates();

    } catch (error) {
        console.error('Error initializing app:', error);
        setLoading(false);
        showError(error.message);

        if (!map) {
            initMap();
        }
    }
}

// Start the app when DOM is ready
if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
} else {
    init();
}
