-- Rates for the three models that actually appear in captured usage.
--
-- model_prices has existed since 0001 and has never had a row in it, so every
-- model call ever ingested priced at zero and every session in the product
-- reports no cost. Zero reads as "this was free" rather than as "we do not
-- know", and it is the one number this system exists to produce.
--
-- PROVENANCE. Every figure below is Anthropic's published list price, read from
-- https://platform.claude.com/docs/en/about-claude/pricing on 2026-08-05. The
-- table there is quoted in dollars per million tokens and per cache tier; the
-- rows here carry the base input and output rates, and the three multipliers
-- that turn the base input rate into the cache tiers:
--
--   model                       base in   5m write   1h write   cache hit   out
--   Claude Fable 5              $10       $12.50     $20        $1          $50
--   Claude Opus 5               $5        $6.25      $10        $0.50       $25
--   Claude Haiku 4.5            $1        $1.25      $2         $0.10       $5
--
-- The cache columns are exactly 1.25x, 2x and 0.1x the base input rate in every
-- row, which is the same ratio the page states as the general rule and the same
-- one server/ingest/cost.go carries as its defaults. They are written out here
-- rather than left to those defaults so that this file can be checked against
-- the published table without reading any Go.
--
-- Nothing is seeded for any other model. A model with no row prices at nothing
-- and the product shows its token counts and says the cost is unpriced, which is
-- the honest answer; a plausible-looking rate invented to fill the gap would
-- produce dollar figures that nobody would think to double-check.
--
-- WHAT effective_from MEANS HERE. It is the date these rates were read, not the
-- date they took effect — the published page states current prices and does not
-- carry the history, so claiming an earlier start would be asserting something
-- that was not checked. Calls older than every row on file are priced at the
-- earliest row rather than at nothing (see Pricer.rateAt), so the backfilled
-- corpus is priced at today's list price. That is an approximation, and it is
-- the one the rate table was designed to make: if these prices are ever found to
-- have changed during the captured period, the correction is an extra row with
-- the older date, not an edit to this one.
--
-- WHY THE IDS ARE SPELLED THIS WAY. model_prices is keyed on the exact string a
-- transcript record carries, which is the pinned model id and not the alias.
-- claude-haiku-4-5-20251001 is what the captured events say, so that is what is
-- written here; "claude-haiku-4-5" would be a row that never matches anything.
--
-- ON CONFLICT DO NOTHING for the same reason store.BootstrapAdmins uses it. Rates are
-- editable at runtime through PutModelPrice, and a deploy that silently
-- reinstated a list price over a negotiated one someone had corrected by hand
-- would be indistinguishable from the correction never having been made.
INSERT INTO model_prices (
  model, effective_from, input_per_mtok, output_per_mtok,
  cache_read_multiplier, cache_write_5m_multiplier, cache_write_1h_multiplier
) VALUES
  ('claude-fable-5',            TIMESTAMPTZ '2026-08-05 00:00:00+00', 10, 50, 0.1, 1.25, 2.0),
  ('claude-opus-5',             TIMESTAMPTZ '2026-08-05 00:00:00+00',  5, 25, 0.1, 1.25, 2.0),
  ('claude-haiku-4-5-20251001', TIMESTAMPTZ '2026-08-05 00:00:00+00',  1,  5, 0.1, 1.25, 2.0)
ON CONFLICT (model, effective_from) DO NOTHING;
