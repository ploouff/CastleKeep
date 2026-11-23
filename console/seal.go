package main

// Password-protected save files for the exchange and the wallets. With
// -password, the program asks for a password and seals its file:
//
//	key  = scrypt(password, salt, N=32768, r=8, p=1) -> 32 bytes
//	file = AES-256-GCM(key, random nonce, the JSON that would have been saved)
//
// The sealed file is JSON too ({"castlekeep":"sealed", ...}), so it can be
// told apart from a plain one. A wrong password fails the GCM check, and
// nothing is loaded or changed. This keeps test keys out of plain sight on
// disk; it is not a substitute for a real wallet's key storage.
//
// Test networks only. Not audited.

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"
	"golang.org/x/crypto/ssh/terminal"
)

const (
	sealN, sealR, sealP = 32768, 8, 1
	minPassword         = 8
)

// sealKey is a password and the key derived from it for one salt.
type sealKey struct {
	pass, salt, key []byte
}

type sealedFile struct {
	Castlekeep string `json:"castlekeep"` // always "sealed"
	KDF        string `json:"kdf"`        // "scrypt"
	N, R, P    int
	Salt       []byte
	Nonce      []byte
	Data       []byte
}

var errWrongPassword = errors.New("wrong password (or the file is damaged)")

func deriveKey(pass, salt []byte, n, r, p int) ([]byte, error) {
	return scrypt.Key(pass, salt, n, r, p, 32)
}

// newSealKey makes a key with a new random salt.
func newSealKey(pass []byte) (*sealKey, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key, err := deriveKey(pass, salt, sealN, sealR, sealP)
	if err != nil {
		return nil, err
	}
	return &sealKey{pass: pass, salt: salt, key: key}, nil
}

// asSealed reports whether data is a sealed file.
func asSealed(data []byte) (*sealedFile, bool) {
	var sf sealedFile
	if json.Unmarshal(data, &sf) != nil || sf.Castlekeep != "sealed" {
		return nil, false
	}
	return &sf, true
}

func (k *sealKey) seal(plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(k.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sf := sealedFile{Castlekeep: "sealed", KDF: "scrypt", N: sealN, R: sealR, P: sealP, Salt: k.salt, Nonce: nonce,
		Data: gcm.Seal(nil, nonce, plain, []byte("castlekeep"))}
	return json.MarshalIndent(sf, "", " ")
}

// unseal opens a sealed file with pass. The key it returns seals later saves
// with the same salt, so the password is only stretched once.
func unseal(sf *sealedFile, pass []byte) ([]byte, *sealKey, error) {
	if sf.KDF != "scrypt" || sf.N <= 1 || sf.N > 1<<20 || sf.R <= 0 || sf.P <= 0 {
		return nil, nil, errors.New("the file uses settings this version doesn't know")
	}
	key, err := deriveKey(pass, sf.Salt, sf.N, sf.R, sf.P)
	if err != nil {
		return nil, nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	if len(sf.Nonce) != gcm.NonceSize() {
		return nil, nil, errWrongPassword
	}
	plain, err := gcm.Open(nil, sf.Nonce, sf.Data, []byte("castlekeep"))
	if err != nil {
		return nil, nil, errWrongPassword
	}
	return plain, &sealKey{pass: pass, salt: sf.Salt, key: key}, nil
}

// readSaved reads a save file, opening it with *k if it is sealed. It returns
// os.ErrNotExist (wrapped) if there is no file.
func readSaved(path string, k *sealKey) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sf, sealed := asSealed(data)
	if !sealed {
		return data, nil
	}
	if k == nil {
		return nil, fmt.Errorf("%s is protected with a password", path)
	}
	plain, _, err := unseal(sf, k.pass)
	return plain, err
}

// sealIfSet seals data when the program has a password.
func sealIfSet(k *sealKey, data []byte) ([]byte, error) {
	if k == nil {
		return data, nil
	}
	return k.seal(data)
}

// ------------------------------------------------------------------ asking for the password

var stdinLines = bufio.NewReader(os.Stdin)

// askPassword reads a password without showing it. CASTLEKEEP_PASSWORD is
// used instead when set (for scripts and tests).
func askPassword(prompt string) ([]byte, error) {
	if p := os.Getenv("CASTLEKEEP_PASSWORD"); p != "" {
		return []byte(p), nil
	}
	fmt.Print(prompt)
	fd := int(os.Stdin.Fd())
	if terminal.IsTerminal(fd) {
		b, err := terminal.ReadPassword(fd)
		fmt.Println()
		return b, err
	}
	// Not a console (input piped in): read a line.
	line, err := stdinLines.ReadString('\n')
	if err != nil && err != io.EOF {
		return nil, err
	}
	return []byte(strings.TrimRight(line, "\r\n")), nil
}

// passwordFor works out the password for a program's save file at start:
//   - a sealed file always needs its password (3 tries);
//   - protect: a new password (asked twice) for a plain or missing file; a
//     plain file is copied aside first, since it holds keys in plain text;
//   - remove: the file is opened with its password and saved plain again.
//
// It returns the key to read the file with, and the key to save with (nil:
// plain).
func passwordFor(path, what string, protect, remove bool) (read, save *sealKey, err error) {
	data, rerr := os.ReadFile(path)
	if rerr == nil {
		if sf, sealed := asSealed(data); sealed {
			for try := 0; try < 3; try++ {
				p, err := askPassword(fmt.Sprintf("Password for %s: ", what))
				if err != nil {
					return nil, nil, err
				}
				if _, k, err := unseal(sf, p); err == nil {
					if remove {
						fmt.Println("The password is removed: the file is saved in plain text from now on.")
						return k, nil, nil
					}
					return k, k, nil
				} else if !errors.Is(err, errWrongPassword) || os.Getenv("CASTLEKEEP_PASSWORD") != "" {
					return nil, nil, err
				}
				fmt.Println("That password doesn't open it.")
			}
			return nil, nil, errWrongPassword
		}
	}
	if remove {
		fmt.Println("The file has no password, so there's nothing to remove.")
	}
	if !protect {
		return nil, nil, nil
	}
	var p []byte
	for {
		p1, err := askPassword(fmt.Sprintf("New password for %s (at least %d characters): ", what, minPassword))
		if err != nil {
			return nil, nil, err
		}
		if len(p1) < minPassword {
			if os.Getenv("CASTLEKEEP_PASSWORD") != "" {
				return nil, nil, fmt.Errorf("CASTLEKEEP_PASSWORD must be at least %d characters", minPassword)
			}
			fmt.Printf("Use at least %d characters.\n", minPassword)
			continue
		}
		p2, err := askPassword("Type it again: ")
		if err != nil {
			return nil, nil, err
		}
		if string(p1) != string(p2) {
			fmt.Println("The two don't match. Try again.")
			continue
		}
		p = p1
		break
	}
	k, err := newSealKey(p)
	if err != nil {
		return nil, nil, err
	}
	if rerr == nil {
		aside := path + ".unprotected-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(aside, data, 0o600); err != nil {
			return nil, nil, fmt.Errorf("couldn't keep a copy of the old file: %v", err)
		}
		fmt.Println("Your old file (keys in plain text) was copied to", aside)
		fmt.Println("Delete that copy yourself once you're happy the password works.")
	}
	return nil, k, nil
}

// startSealed is startFile with passwords: -fresh first, then the password
// (if the file has one, or protect/remove asks for a change), then loading.
// setSeal sets the program's key: the reading key while loading, then the
// key it saves with from now on.
func startSealed(path, what string, fresh, protect, remove bool, setSeal func(*sealKey), load func(string) (bool, error)) bool {
	if fresh {
		if old, err := setAside(path); err != nil {
			fmt.Println("Couldn't move the old file aside:", err)
			os.Exit(1)
		} else if old != "" {
			fmt.Println("Started fresh. The old file is now", old)
		}
	}
	rk, sk, err := passwordFor(path, what, protect, remove)
	if err != nil {
		fmt.Println("Couldn't open the saved data:", err)
		os.Exit(1)
	}
	setSeal(rk)
	loaded := startFile(path, false, load)
	setSeal(sk)
	if sk != nil {
		fmt.Println("The saved file is protected with your password.")
	}
	return loaded
}

// savedHow says how a program's file is kept, for its start message.
func savedHow(k *sealKey) string {
	if k != nil {
		return "(protected with your password; test coins only)"
	}
	return "(private keys in plain text: test coins only; start with -password to protect it)"
}
