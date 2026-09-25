-- The admins table the governed views read (views.sql): a person whose
-- signed-in address is listed here sees every row of every view; everyone
-- else sees the rows that name them in viewer_emails.
--
-- A table, because a view's WHERE can test SESSION_USER() against a column
-- but not against a Google group's membership, and the loaded tables can
-- carry no row access policy that survives WRITE_TRUNCATE for certain
-- (views.sql says why). It lives in the raw dataset, beside the tables it
-- governs, where the members grantee has no access entry; the export
-- service account can write it (it is WRITER there), which is why the
-- runbook's post-run checks read the list back.
--
-- CREATE OR REPLACE ... AS SELECT: the whole table is the list, so a
-- provision run makes it exactly --admin-emails and nothing else, and an
-- address removed from the flag stops seeing other people's rows on the
-- next run. __ADMIN_EMAILS__ is rendered by provision.sh as a quoted,
-- comma-separated list; the default is a placeholder address that matches
-- nobody, which is the safe failure (nobody is admin until the owner names
-- them). List the address a person signs in to BigQuery with; if in doubt,
-- list both Workspace addresses.

CREATE OR REPLACE TABLE `__PROJECT__.__RAW_DATASET__.admins`
OPTIONS (description = 'Signed-in addresses that see every row through the loop_sessions views (examples/deploy-gcp/analytics/provision.sh --admin-emails)')
AS SELECT email FROM UNNEST([__ADMIN_EMAILS__]) AS email;
