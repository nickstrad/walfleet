-- Run-dependent values of experiment 1.1: where the CLI was found and how its
-- other compile options differ from the driver's. The evidence check skips them.

.print
.print '── Info: the CLI of this machine ───────────────────────────────────────────'
.print
.print 'The CLI that was used, and the compile options only one build has. These'
.print 'differences do not touch the file format, and differ between CLI builds.'

SELECT path, origin FROM info_cli ORDER BY path;

SELECT side, unnest(string_split(options, ' ')) AS option
FROM info_option_differences ORDER BY side, option;
