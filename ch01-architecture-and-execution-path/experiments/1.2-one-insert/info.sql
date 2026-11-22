-- Run-dependent values of experiment 1.2: where the work directory is, which
-- sync syscall each build used, and where the CLI first asks for randomness.
-- The evidence check skips them.

.print
.print '── Info: this machine ──────────────────────────────────────────────────'
.print
.print 'The work directory, and the sync syscall of each build. The Go driver uses'
.print 'fsync; a CLI build may use fdatasync, which the twin check treats as the same.'

SELECT key, value FROM info_run ORDER BY key;

SELECT mode, go_driver, cli, cli_path FROM info_sync ORDER BY mode;

.print
.print 'SQLite seeds its random generator on first use, which differs between CLI'
.print 'builds; the VFS call that asks for randomness is left out of the phases:'

SELECT mode, phase_no, vfs_call FROM info_randomness ORDER BY mode, phase_no;
