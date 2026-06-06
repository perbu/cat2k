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

// Recent-trail window. Mirrors the radar's server-side history window
// (api.go: historyStart = now - 3h) so the map shows the same "where it just
// went" path as status.html.
const TRAIL_MAX_AGE_HOURS = 3;

// State
let map = null;
let heatLayers = {};         // trackerId -> L.heatLayer
let trailLayers = {};        // trackerId -> L.layerGroup of polyline segments
let latestMarkers = {};      // trackerId -> L.marker
let allTrackers = [];
let currentDateRange = { days: 7 };
let positionsByTracker = {}; // trackerId -> positions[]
let heatMapSettings = {
    intensity: 0.3,
    radius: 25,
    blur: 15
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

// Style for one trail segment, faded by the age of its midpoint. Recent
// segments show the tracker's color at full strength; older ones fade toward
// dark and translucent so the freshest path reads clearly on the light map.
function trailSegmentStyle(baseHex, midTimeMs) {
    const ageHours = (Date.now() - midTimeMs) / (1000 * 60 * 60);
    const fade = Math.min(ageHours / TRAIL_MAX_AGE_HOURS, 1);
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

// Update statistics panel
function updateStats() {
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

    document.getElementById('stat-count').textContent = total || '-';

    if (range.start && range.end) {
        const startStr = range.start.toLocaleDateString();
        const endStr = range.end.toLocaleDateString();
        document.getElementById('stat-range').textContent = `${startStr} - ${endStr}`;
    }

    document.getElementById('stat-latest').textContent = latestOverall ? latestOverall.toLocaleString() : '-';
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

// Populate tracker legend (color swatch + name)
function populateTrackerLegend(trackers) {
    const legend = document.getElementById('tracker-legend');
    legend.innerHTML = '';

    if (trackers.length === 0) {
        legend.innerHTML = '<div class="tracker-legend-item">No trackers found</div>';
        return;
    }

    const sorted = [...trackers].sort((a, b) => a.id - b.id);
    sorted.forEach((tracker) => {
        const swatch = paletteSwatch(paletteForIndex(trackerPaletteIndex(tracker.id)));
        const item = document.createElement('div');
        item.className = 'tracker-legend-item';
        item.innerHTML = `
            <span class="tracker-legend-swatch" style="background: ${swatch}"></span>
            ${tracker.name}
        `;
        legend.appendChild(item);
    });
}

// Format a distance in metres for display.
function formatDistance(m) {
    if (m >= 1000) return `${(m / 1000).toFixed(1)} km`;
    return `${Math.round(m)} m`;
}

// Fetch per-cat activity summary
async function fetchActivity(days = 4) {
    const response = await fetch(`${API_BASE}/api/activity?days=${days}`);
    if (!response.ok) {
        throw new Error(`Failed to fetch activity: ${response.statusText}`);
    }
    return response.json();
}

// Render the per-cat activity panel. Colors match the tracker legend on this
// page: swatch by palette, indexed in id-sorted order.
function renderActivity(data) {
    const panel = document.getElementById('activity-panel');
    panel.innerHTML = '';

    const trackers = [...(data.trackers || [])].sort((a, b) => a.id - b.id);
    if (trackers.length === 0) {
        panel.innerHTML = '<div class="activity-empty">No activity data</div>';
        return;
    }

    const caption = document.createElement('div');
    caption.className = 'activity-caption';
    caption.textContent = 'Distance travelled per day';
    panel.appendChild(caption);

    const dayLabel = (date) => new Date(date + 'T00:00:00')
        .toLocaleDateString(undefined, { weekday: 'short' });

    trackers.forEach((tracker) => {
        const color = paletteSwatch(paletteForIndex(trackerPaletteIndex(tracker.id)));
        const days = tracker.days || [];
        const today = days[days.length - 1];
        const maxDist = Math.max(1, ...days.map(d => d.distance_m));

        const bars = days.map((d, i) => {
            const isToday = i === days.length - 1;
            const pct = Math.round((d.distance_m / maxDist) * 100);
            // Day stats are carried on the element for the hover popup.
            const data = `data-name="${tracker.name}" data-date="${d.date}"`
                + ` data-dist="${d.distance_m}" data-range="${d.range_m}"`
                + ` data-active="${d.active_fraction}" data-fixes="${d.fixes}"`;
            return `<div class="activity-bar ${isToday ? 'today' : ''}" ${data}>
                <div class="activity-bar-fill" style="height: ${pct}%; background: ${color}"></div>
            </div>`;
        }).join('');

        const labels = days.map((d, i) => {
            const isToday = i === days.length - 1;
            const text = isToday ? 'today' : dayLabel(d.date);
            return `<div class="activity-daylabel ${isToday ? 'today' : ''}">${text}</div>`;
        }).join('');

        const todayDist = today ? formatDistance(today.distance_m) : '–';

        const cat = document.createElement('div');
        cat.className = 'activity-cat';
        cat.innerHTML = `
            <div class="activity-head">
                <span class="tracker-legend-swatch" style="background: ${color}"></span>
                <span class="activity-name">${tracker.name}</span>
                <span class="activity-today">${todayDist} <small>today</small></span>
            </div>
            <div class="activity-bars">${bars}</div>
            <div class="activity-labels">${labels}</div>
        `;
        panel.appendChild(cat);
    });

    attachActivityTooltip(panel);
}

// Single floating popup, shared by all bars, shown on hover.
function attachActivityTooltip(panel) {
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

// Load and render activity, swallowing errors (it's a secondary panel)
async function loadActivity() {
    try {
        const data = await fetchActivity(7);
        renderActivity(data);
    } catch (err) {
        console.error('Error loading activity:', err);
        document.getElementById('activity-panel').innerHTML =
            '<div class="activity-empty">Activity unavailable</div>';
    }
}

// Initialize the map
function initMap(center = [59.9139, 10.7522], zoom = 13) {
    if (map) {
        return;
    }

    map = L.map('map').setView(center, zoom);

    L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
        attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors',
        maxZoom: 19
    }).addTo(map);
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

// Draw the recent-movement trail per tracker: consecutive fixes within the
// trail window joined as age-faded line segments, layered above the heatmap.
// Reuses the positions already in positionsByTracker — no extra API call.
function renderTrails() {
    Object.values(trailLayers).forEach(group => map.removeLayer(group));
    trailLayers = {};

    const cutoff = Date.now() - TRAIL_MAX_AGE_HOURS * 60 * 60 * 1000;

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

        group.addTo(map);
        trailLayers[id] = group;
    });
}

// Render one heat layer per tracker, each with its assigned palette
function renderHeatMaps(resetView = true) {
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
            map.setView(center, 13);
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
        updateStats();

        setLoading(false);
    } catch (error) {
        console.error('Error loading tracker data:', error);
        setLoading(false);
        showError(error.message);
    }
}

// Setup event listeners
function setupEventListeners() {
    // Date range preset buttons
    document.querySelectorAll('.date-presets .btn').forEach(btn => {
        btn.addEventListener('click', () => {
            document.querySelectorAll('.date-presets .btn').forEach(b => b.classList.remove('active'));
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

    // Heat map setting sliders — re-render with cached positions, no refetch
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

// Main initialization function
async function init() {
    try {
        setLoading(true);

        initializeDateInputs();
        setupEventListeners();

        allTrackers = await fetchTrackers();

        if (!allTrackers || allTrackers.length === 0) {
            throw new Error('No trackers found');
        }

        console.log(`Found ${allTrackers.length} tracker(s)`);

        populateTrackerLegend(allTrackers);
        loadActivity();

        await loadAllTrackerData();

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
