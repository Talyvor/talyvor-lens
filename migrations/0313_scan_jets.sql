-- 0313_scan_jets.sql — B52.30: Scan beta, company jets (behind LENS_SCAN_JETS, which stays off).
--
-- scan_jet_exclusions keeps an aircraft out of every count: an erasure request, an aircraft used by one person, a LADD
-- or a PIA aircraft. scan_jet_aircraft records which aircraft each stored company-day figure counted, only so that an
-- exclusion can delete every figure the aircraft was part of; a figure itself never names an aircraft.
CREATE TABLE IF NOT EXISTS scan_jet_exclusions (
    registration  TEXT PRIMARY KEY CHECK (registration ~ '^N[0-9A-Z]+$'),
    reason        TEXT NOT NULL CHECK (reason IN ('erasure', 'single-user', 'ladd', 'pia')),
    noted_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS scan_jet_aircraft (
    point_id      BIGINT NOT NULL REFERENCES scan_points(id) ON DELETE CASCADE,
    registration  TEXT NOT NULL,
    PRIMARY KEY (point_id, registration)
);
CREATE INDEX IF NOT EXISTS scan_jet_aircraft_registration_idx ON scan_jet_aircraft (registration);
