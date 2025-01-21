package dynamic_extractor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/traP-jp/h24w-17/testutil"
)

func TestMain(m *testing.M) {
	testutil.InitialSetupDB(m)
}

func TestExtract(t *testing.T) {
	db := testutil.SetupMysqlDB(t, "mysql+analyzer")
	defer db.Close()

	_, err := db.Exec(`CREATE TABLE users (id INT, name VARCHAR(255))`)
	assert.NoError(t, err)

	_, err = db.Exec(`INSERT INTO users (id, name) VALUES (1, 'Alice')`)
	assert.NoError(t, err)

	rows, err := db.Query(`SELECT * FROM users`)
	assert.NoError(t, err)
	defer rows.Close()
	type User struct {
		ID   int
		Name string
	}
	var users []User
	for rows.Next() {
		var user User
		err = rows.Scan(&user.ID, &user.Name)
		assert.NoError(t, err)
		users = append(users, user)
	}
	assert.Equal(t, []User{{1, "Alice"}}, users)

	queries := getQueries()
	expected := []string{
		`CREATE TABLE users (id INT, name VARCHAR(255))`,
		`INSERT INTO users (id, name) VALUES (1, 'Alice')`,
		`SELECT * FROM users`,
	}
	assert.Equal(t, expected, queries)
}
