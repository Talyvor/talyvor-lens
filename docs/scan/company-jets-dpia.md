# Company jets beta — data protection impact assessment (DRAFT, for Nick's approval)

Status: draft written with B52.30. The beta stays off (`LENS_SCAN_JETS`) until this assessment and the
legitimate-interest assessment beside it are approved, and a FlightAware AeroAPI Standard key is on the server.

## 1. What the processing is

Scan counts, for each named company, how many flights its company-owned jets made on each day and at which airports
they landed. A figure is always a company's day ("Example Industries Inc: 2 flights on 1 Nov, 2 landing at KPBI"),
never an aircraft's, and never less than 24 hours old: only UTC days that ended at least 24 hours before the run are
counted. Reports may use these counts as one input among others.

Sources:
- FAA aircraft registry (releasable database, daily download, public domain): who an aircraft is registered to.
- FlightAware AeroAPI Standard (licence allows showing data to the public): flights by registration. FlightAware takes
  FAA data, so aircraft on the FAA's LADD list are blocked by FlightAware before Talyvor sees them.

## 2. Why it may be personal data

A tail number with a position is personal data when the owner, or the one person who uses the aircraft, can be
identified (Swedish IMY on Flightradar24, June 2025). A company aggregate can still point at a person when the company
has one aircraft that one executive uses. Jet tracking has been described as stalking (2023).

## 3. What is never processed

- Aircraft registered to individuals, partnerships, co-owners or governments (FAA registrant types 1, 2, 4, 5, 9).
- Aircraft held in trust (a registrant name with TRUST, TRUSTEE, TR, TRS or TTEE).
- Registrations whose owner details are withheld (a blank or "WITHHELD" name).
- LADD aircraft: FlightAware blocks them; a flight marked blocked is dropped; an aircraft can also be excluded as LADD.
- PIA aircraft: a flight that does not carry the registration asked for is dropped; Talyvor never tries to link a PIA
  address to an owner.
- Aircraft known to be used by one person: excluded by the operator (reason `single-user`).
- Named people: Scan never tracks a person, and no figure names one.

## 4. What is stored

- `scan_points`: one figure per company and day (flights; arrivals per airport), with its provenance. No registration.
- `scan_jet_aircraft`: which registrations each figure counted. Kept only so an exclusion can delete every figure an
  aircraft was part of; never shown and never read by a report.
- `scan_jet_exclusions`: excluded registrations and why (erasure, single-user, ladd, pia).
- No positions, routes, times of day or raw AeroAPI responses are stored: only their SHA-256.

## 5. Rights

An erasure or objection request about an aircraft is handled by the operator at `POST /v1/admin/scan/jets/exclusions`
with reason `erasure`: the aircraft is never counted again, and every stored figure it was part of is deleted (with
anything derived from them), including a figure a run was storing at that moment. The next run recounts those days without it, where AeroAPI still
holds them (ten days).

## 6. Risks and measures

| Risk | Measure |
|---|---|
| A company's single jet identifies its CEO's movements | Exclusion `single-user`; open question 1 below |
| Real-time tracking of a person | 24-hour delay on complete days; no positions stored |
| Unmasking PIA | Only flights carrying the registration asked for are counted; no other matching |
| An owner who opted out through LADD | FlightAware applies LADD; blocked flights dropped |
| Use beyond reports | Figures carry provenance; the source register's display flags stay off until approved |

## 7. Open questions for Nick

1. Should a company need a minimum number of counted aircraft before its figures are shown? The item names no number,
   so none is applied.
2. Which companies to name in `LENS_SCAN_JETS_COMPANIES` (each costs one AeroAPI request per aircraft per day).
3. The register entries for FlightAware and the FAA registry keep public display off until this is approved.
4. Whether to answer an individual's request without proof that the aircraft is theirs (the draft assumes yes).
