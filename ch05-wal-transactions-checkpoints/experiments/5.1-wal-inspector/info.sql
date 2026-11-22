-- Values that differ on every run: the random salts, the checksums that
-- depend on them, and how many bytes differ between run 1 and run 2.

.print
.print '── info. Salts and checksums ──────────────────────────────────────────'

SELECT run, printf('0x%08x', salt1) AS salt1, printf('0x%08x', salt2) AS salt2, header_checksum FROM info_salts ORDER BY run;

SELECT region, differing_bytes FROM info_region_bytes ORDER BY seq;
