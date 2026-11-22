-- Run-dependent values of experiment 7.1: where the CLI build first asked the
-- VFS for random bytes. The evidence check skips them.

.print
.print '── Info: xRandomness in this CLI build ─────────────────────────────────'
.print
.print 'Where xRandomness ran, as the seq of the stream line just before it. An'
.print 'installed CLI asks right after opening the database; a ./configure build'
.print 'of the pin right before the first journal header, whose nonce is random.'

SELECT after_seq, call FROM info_randomness ORDER BY after_seq;
