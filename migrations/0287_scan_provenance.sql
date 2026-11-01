-- 0287_scan_provenance.sql — B52.20: Scan, part 1. Every number knows where it came from.
--
-- scan_sources is the source register: each data source Scan may read, with what its licence allows (commercial use,
-- display to the public, redistribution of derived figures), the attribution text it requires, its cost, its rate
-- limit and an on/off switch. A source the research rules out carries refused_reason and can never be switched on.
-- Every source starts off; a collector (B52.21 onwards) switches its own source on.
--
-- scan_points holds every figure Scan stores. An observed point carries its source, the licence it was retrieved
-- under, the retrieval time, the SHA-256 of the content it was read from and every transformation applied to it. A
-- derived point has no source of its own: it lists every input it came from in scan_point_inputs.

CREATE TABLE IF NOT EXISTS scan_sources (
    id                     TEXT PRIMARY KEY,
    name                   TEXT NOT NULL,
    licence                TEXT NOT NULL,
    commercial_use         BOOLEAN NOT NULL DEFAULT FALSE,
    public_display         BOOLEAN NOT NULL DEFAULT FALSE,
    redistribute_derived   BOOLEAN NOT NULL DEFAULT FALSE,
    attribution            TEXT NOT NULL DEFAULT '',
    cost                   TEXT NOT NULL DEFAULT '',
    requests_per_minute    INTEGER,
    enabled                BOOLEAN NOT NULL DEFAULT FALSE,
    refused_reason         TEXT,
    CONSTRAINT scan_sources_refused_stay_off CHECK (refused_reason IS NULL OR NOT enabled)
);

CREATE TABLE IF NOT EXISTS scan_points (
    id            BIGSERIAL PRIMARY KEY,
    kind          TEXT NOT NULL CHECK (kind IN ('observed', 'derived')),
    series        TEXT NOT NULL CHECK (series <> ''),
    observed_at   TIMESTAMPTZ NOT NULL,
    value         DOUBLE PRECISION NOT NULL,
    unit          TEXT NOT NULL CHECK (unit <> ''),
    source_id     TEXT REFERENCES scan_sources(id),
    licence       TEXT,
    retrieved_at  TIMESTAMPTZ,
    content_hash  TEXT,
    transforms    JSONB NOT NULL CHECK (jsonb_typeof(transforms) = 'array'),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT scan_points_observed_provenance CHECK (kind <> 'observed' OR (
        source_id IS NOT NULL AND licence IS NOT NULL AND licence <> '' AND retrieved_at IS NOT NULL
        AND content_hash ~ '^[0-9a-f]{64}$')),
    CONSTRAINT scan_points_derived_has_method CHECK (kind <> 'derived' OR (
        source_id IS NULL AND jsonb_array_length(transforms) > 0))
);
CREATE INDEX IF NOT EXISTS scan_points_series_idx ON scan_points (series, observed_at);

CREATE TABLE IF NOT EXISTS scan_point_inputs (
    point_id  BIGINT NOT NULL REFERENCES scan_points(id),
    input_id  BIGINT NOT NULL REFERENCES scan_points(id),
    PRIMARY KEY (point_id, input_id),
    CHECK (point_id <> input_id)
);

-- The data catalogue of the research of 1 Nov 2026. A licence right the research does not confirm stays FALSE.
INSERT INTO scan_sources (id, name, licence, commercial_use, public_display, redistribute_derived, attribution, cost, requests_per_minute, refused_reason) VALUES
    ('gie-agsi', 'GIE AGSI+ (EU gas storage)', 'GIE transparency platform terms (commercial reuse not stated: internal only)', FALSE, FALSE, FALSE, 'Source: GIE AGSI+', 'free', NULL, NULL),
    ('gie-alsi', 'GIE ALSI (EU LNG terminals)', 'GIE transparency platform terms (commercial reuse not stated: internal only)', FALSE, FALSE, FALSE, 'Source: GIE ALSI', 'free', NULL, NULL),
    ('entsog', 'ENTSOG transparency platform (gas flows)', 'ENTSOG transparency platform terms, credit required', TRUE, TRUE, TRUE, 'Source: ENTSOG Transparency Platform', 'free', NULL, NULL),
    ('eia', 'US EIA open data (weekly petroleum stocks)', 'US government public domain, credit required', TRUE, TRUE, TRUE, 'Source: U.S. Energy Information Administration', 'free', NULL, NULL),
    ('ecmwf-open', 'ECMWF open real-time forecasts', 'CC BY 4.0', TRUE, TRUE, TRUE, 'Source: ECMWF open data, CC BY 4.0', 'free', NULL, NULL),
    ('usda-nass', 'USDA NASS Quick Stats', 'US government public domain', TRUE, TRUE, TRUE, 'Source: USDA National Agricultural Statistics Service', 'free', NULL, NULL),
    ('usda-wasde', 'USDA WASDE report', 'US government public domain', TRUE, TRUE, TRUE, 'Source: USDA World Agricultural Supply and Demand Estimates', 'free', NULL, NULL),
    ('jrc-mars', 'EU JRC MARS bulletins', 'Public, credit required', TRUE, TRUE, TRUE, 'Source: European Commission, JRC MARS Bulletins', 'free', NULL, NULL),
    ('copernicus-sentinel', 'Copernicus Sentinel-1 and Sentinel-2', 'Copernicus data licence: use and redistribution with credit', TRUE, TRUE, TRUE, 'Contains modified Copernicus Sentinel data', 'free', NULL, NULL),
    ('usgs-landsat', 'USGS Landsat', 'US government public domain, credit required', TRUE, TRUE, TRUE, 'Landsat imagery courtesy of the U.S. Geological Survey', 'free', NULL, NULL),
    ('nasa-black-marble', 'NASA Black Marble night lights', 'NASA open data', TRUE, TRUE, TRUE, 'Source: NASA Black Marble', 'free', NULL, NULL),
    ('sec-edgar', 'SEC EDGAR (Form 4, 13F)', 'US government public domain', TRUE, TRUE, TRUE, 'Source: U.S. Securities and Exchange Commission, EDGAR', 'free', 600, NULL),
    ('usaspending', 'USAspending contract awards', 'US government public domain', TRUE, TRUE, TRUE, 'Source: USAspending.gov', 'free', NULL, NULL),
    ('faa-registry', 'FAA aircraft registry', 'US government public domain', TRUE, FALSE, FALSE, 'Source: Federal Aviation Administration aircraft registry', 'free', NULL, NULL),
    ('flightaware-standard', 'FlightAware AeroAPI Standard', 'FlightAware AeroAPI Standard tier (B2C use allowed)', TRUE, FALSE, FALSE, 'Source: FlightAware', '$100 a month minimum', NULL, NULL),
    ('opensky', 'OpenSky Network', 'Research only; commercial use needs a licence', FALSE, FALSE, FALSE, '', '', NULL, 'ruled out by the Scan research of 1 Nov 2026: no commercial licence'),
    ('adsb-exchange', 'ADS-B Exchange', 'No written licence', FALSE, FALSE, FALSE, '', 'price on request', NULL, 'ruled out by the Scan research of 1 Nov 2026: no written licence, not bound by LADD'),
    ('quiver', 'Quiver Quantitative', 'Personal use only', FALSE, FALSE, FALSE, '', '', NULL, 'ruled out by the Scan research of 1 Nov 2026: licensed for personal use only'),
    ('global-fishing-watch', 'Global Fishing Watch', 'Non-commercial only', FALSE, FALSE, FALSE, '', '', NULL, 'ruled out by the Scan research of 1 Nov 2026: non-commercial only'),
    ('aishub-raw', 'AISHub (raw)', 'Free only to receiver feeders; no raw redistribution', FALSE, FALSE, FALSE, '', '', NULL, 'ruled out by the Scan research of 1 Nov 2026: no raw redistribution')
ON CONFLICT (id) DO NOTHING;
