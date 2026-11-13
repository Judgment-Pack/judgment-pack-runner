package runner

// A test that reads or changes the sqlite kind's rows below the contract
// reaches its database through the Service's store; the Service holds no
// database of its own (docs/design/store-interface.md, section 9.1).

import "database/sql"

// sqliteDB is the database of s's store, which is the sqlite kind, alone or
// under the twice mode.
func sqliteDB(s *Service) *sql.DB { return underlying(s.store).(*sqliteStore).db }
