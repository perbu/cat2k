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

// State
let map = null;
let heatLayers = {};         // trackerId -> L.heatLayer
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

function paletteSwatch(palette) {
    // Pick a mid-to-high stop so the swatch matches what the heatmap actually shows
    const stops = Object.keys(palette).map(Number).sort((a, b) => a - b);
    const mid = stops[Math.floor(stops.length * 0.7)] ?? stops[stops.length - 1];
    return palette[mid] ?? '#888';
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
    sorted.forEach((tracker, idx) => {
        const swatch = paletteSwatch(paletteForIndex(idx));
        const item = document.createElement('div');
        item.className = 'tracker-legend-item';
        item.innerHTML = `
            <span class="tracker-legend-swatch" style="background: ${swatch}"></span>
            ${tracker.name}
        `;
        legend.appendChild(item);
    });
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

// Render one heat layer per tracker, each with its assigned palette
function renderHeatMaps(resetView = true) {
    Object.values(heatLayers).forEach(layer => map.removeLayer(layer));
    heatLayers = {};
    Object.values(latestMarkers).forEach(marker => map.removeLayer(marker));
    latestMarkers = {};

    const ids = sortedTrackerIds();
    let allPositions = [];

    ids.forEach((id, idx) => {
        const positions = positionsByTracker[id] || [];
        if (positions.length === 0) return;

        const palette = paletteForIndex(idx);
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
