# Platform reporting and the VAT return figures

Once a year Talyvor tells the tax authorities about the sellers its marketplace paid: the UK's reporting rules for
digital platforms and the EU's DAC7 ask a platform for each seller's identification and, quarter by quarter, what it
paid them. Each quarter Talyvor also files a UK VAT return and an EU OSS return for the VAT it charged buyers. Lens
produces the figures for both. Talyvor's accountant decides what is filed.

## The annual export

```
lens platform-report --year 2026 [--test] [--out <dir>] [--operator <name>]
```

Run it inside the lens container: it reads the same Postgres as the server and opens the sellers' sealed details with
the same `LENS_PROVIDER_SECRET_KEK`. It writes `platform-report-2026.csv` and `platform-report-2026.json` to `--out`
(the current directory by default) and prints each file's record count and sha256. `--test` reports sandbox funds instead,
into `platform-report-2026-test.*`. `--operator` names who ran it (default `$USER`).

The operator can also download one file at a time with the global admin key:

```
POST /v1/admin/platform-reports   {"year": 2026, "funding": "live", "format": "csv", "actor": "<operator>"}
GET  /v1/admin/platform-reports?year=2026   the files written, with their operator, record count and sha256
```

`funding` is `live` (the default) or `test`, `format` is `csv` (the default) or `json`. The POST answers with the file
and its id, sha256 and record count in the `X-Platform-Report-Id`, `X-Platform-Report-Sha256` and
`X-Platform-Report-Rows` headers.

**The files hold the sellers' TINs, dates of birth and account numbers in clear.** They are opened only to be written
into the file. Hand the file to Talyvor's accountant and nobody else, and delete local copies when it has been filed.

### Who is in it

One record per reportable seller and activity. A seller is reportable when the country of residence in their seller
tax details is the United Kingdom or an EU member state. Sellers resident elsewhere are left out. A seller credited in
the year who has not given a country is not in the records: the JSON lists them under `unresolved_sellers`, and the
command prints them, because nobody can yet say whether they are reportable. They are asked for their details, and
their payouts held after the second reminder, until they give them.

### The records

| field | what it is |
| --- | --- |
| `year`, `funding`, `currency` | the year reported, `live` or `test` money, and `USD` |
| `workspace_id` | the seller's workspace |
| `seller_type` | `individual` or `entity` |
| `first_name`, `middle_name`, `last_name` | an individual's names |
| `legal_name` | an entity's legal name |
| `primary_address` | the seller's primary address |
| `country_of_residence` | two letters, such as `GB` or `DE` |
| `tins` | every TIN with the jurisdiction that issued it, in clear: `GB:1234567890;DE:12345678901` |
| `date_of_birth` | an individual's, `YYYY-MM-DD`, in clear |
| `company_registration_number` | an entity's |
| `vat_number` | as the tax partner normalised it |
| `financial_account_identifier` | the IBAN or account number the seller is paid to, in clear |
| `financial_account_holder` | its holder |
| `details_complete` | whether the seller's tax details are complete |
| `activity` | what the money was for: `digital_listing` (a use, buy, rent or subscription of a listing), `agent_payment` (a payment to another company's agent) and, once tasks for people ship, `personal_service` |
| `q1_…` to `q4_…`, `total_…` | the quarter's figures, and the year's |

Each quarter (UTC) has four figures, all integers in µUSD (millionths of a US dollar, the marketplace journal's unit):

- `consideration_usd_micros` — what was credited to the seller in the quarter: their share of each sale, and the
  royalties and split shares their listings earned from others' sales, less what refunds and chargebacks took back.
  It is after Talyvor's fee and before tax.
- `activities` — how many sales and royalties it was credited for.
- `fees_usd_micros` — Talyvor's fee on the seller's own sales in the quarter, less refunded fees.
- `taxes_withheld_usd_micros` — always 0: Talyvor withholds no tax from sellers.

Every figure comes from the marketplace journal: the clear and reversal entries in the quarter that credited the
seller's holdback, and the fee postings of their own sales. The JSON file has the same records, each with a `quarters` array and a `total`.

### The record of each run

Each file written is a row in `platform_reports` — year, funding, format, when it was generated, the operator, the
number of records and the file's sha256 — and an entry in the operator audit trail (`platform_report.export`). Neither
holds anything from inside the file. To show which run produced a file, compare `sha256sum` of the file with the row.

## The VAT return figures

```
lens tax return --jurisdiction GB --quarter 2026Q4 [--test] [--json]
lens tax return --jurisdiction EU --quarter 2026Q4      every member state, for the OSS return
lens tax return --jurisdiction DE --quarter 2026Q4      one member state
```

The quarter's figures, by jurisdiction, treatment (`standard`, `reverse_charge`, `zero`, …) and rate:

- `sales`, `taxable`, `tax` — the tax lines of the sales whose buyer's invoice was paid in the quarter;
- `refunds`, `refunded_taxable`, `refunded_tax` — the tax lines of the sales refunded in the quarter;
- `net_taxable`, `net_tax` — the first less the second; the totals are their sums.

Amounts are µUSD. Below them is what the journal's `tax:<country>` accounts were credited in the quarter: the two agree
unless a sale cleared before its tax was billed, which the command flags. Converting to pounds or euros for the return,
and deciding which boxes each figure goes in, is the accountant's.
