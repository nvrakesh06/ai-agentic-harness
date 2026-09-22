package safety

import "testing"

func TestSecretsAndPaths(t *testing.T) {
	for _, p := range []string{".env", "dir/.env.production", "../escape", "id_rsa", "state.db"} {
		if Path(p) == nil {
			t.Fatal(p)
		}
	}
	if Path(".env.example") != nil {
		t.Fatal("example rejected")
	}
	secret := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	if Check(secret) == nil || Redact(secret) == secret {
		t.Fatal("credential leaked")
	}
	jsonSecret := `{"password":"` + "abcdefghijklmnopqrstuvwx" + `"}`
	if Check(jsonSecret) == nil {
		t.Fatal("JSON credential field accepted")
	}
}
