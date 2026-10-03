-- The user a remote-only bd connects as, with the grants the production
-- server gives it. The password is fixed and test-only: the server is
-- reachable from 127.0.0.1 alone. scripts/test-dolt.sh repeats it.
CREATE USER 'beads'@'%' IDENTIFIED BY 'bd-test';
-- Granted last: scripts/test-dolt.sh treats a beads login that can read
-- mysql.user as the end of initialisation.
GRANT ALL PRIVILEGES ON *.* TO 'beads'@'%' WITH GRANT OPTION;
