package fs

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"

	"github.com/pkg/errors"
)

// AssertPathExists returns nil only if it has been successfully
// verified that all specified paths exists.
func AssertPathExists(paths ...string) error {
	for _, p := range paths {
		exist, err := PathExists(p)
		if err != nil {
			return err
		}
		if !exist {
			return errors.Errorf("Path %s does not exist", p)
		}
	}
	return nil
}

func AssertPathNotExists(paths ...string) error {
	for _, p := range paths {
		exist, err := PathExists(p)
		if err != nil {
			return err
		}
		if exist {
			return errors.Errorf("Path %s exists but should not", p)
		}
	}
	return nil
}

// PathExists checks if the specified path exists.
func PathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}

func EnsureDirectoryExists(path string) error {
	exists, err := PathExists(path)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return os.MkdirAll(path, 0700)
}

func Empty(path string) bool {
	matches, _ := filepath.Glob(filepath.Join(path, "*"))
	return len(matches) == 0
}

func Copy(src, dest string) error {
	exists, err := PathExists(src)
	if err != nil || !exists {
		return err
	}
	bts, err := ioutil.ReadFile(src)
	if err != nil {
		return err
	}
	return SaveFile(dest, bts)
}

// Save the filecontents at the specified path atomically:
// - first save the content in a temp file with a random filename in the same dir
// - then rename the temp file to the specified filepath, overwriting the old file
func SaveFile(fpath string, content []byte) (err error) {
	dir := path.Dir(fpath)

	// Read random data for filename and convert to hex
	randBytes := make([]byte, 16)
	_, err = rand.Read(randBytes)
	if err != nil {
		return
	}
	tempfilename := hex.EncodeToString(randBytes)

	// Create temp file
	err = ioutil.WriteFile(filepath.Join(dir, tempfilename), content, 0600)
	if err != nil {
		return
	}

	// Rename, overwriting old file
	return os.Rename(filepath.Join(dir, tempfilename), fpath)
}

func CopyDirectory(src, dest string) error {
	if err := EnsureDirectoryExists(dest); err != nil {
		return err
	}

	return filepath.Walk(src, filepath.WalkFunc(
		func(path string, info os.FileInfo, err error) error {
			if path == src {
				return nil
			}
			subpath := path[len(src):]
			if info.IsDir() {
				if err := EnsureDirectoryExists(dest + subpath); err != nil {
					return err
				}
			} else {
				srcfile, err := os.Open(path)
				if err != nil {
					return err
				}
				defer srcfile.Close()
				bts, err := ioutil.ReadAll(srcfile)
				if err != nil {
					return err
				}
				if err := SaveFile(dest+subpath, bts); err != nil {
					return err
				}
			}
			return nil
		}),
	)
}

// ReadKey returns either the content of the file specified at path, if it exists,
// or []byte(key) otherwise. It is an error to specify both or none arguments, or
// specify an empty or unreadable file. If there is no error then the return []byte is non-empty.
func ReadKey(key, path string) ([]byte, error) {
	if (key != "" && path != "") || (key == "" && path == "") {
		return nil, errors.New("provide either key or path to key")
	}

	var bts []byte

	if path == "" {
		bts = []byte(key)
	} else {
		stat, err := os.Stat(path)
		if err != nil {
			return nil, errors.New("no key found at specified path")
		}
		if stat.IsDir() {
			return nil, errors.New("cannot read key from a directory")
		}
		bts, err = ioutil.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}

	if len(bts) == 0 {
		return nil, errors.New("empty key provided")
	}
	return bts, nil
}

// Base64Decode decodes the specified bytes as any of the Base64 dialects:
// standard encoding (+, /) and URL encoding (-, _), with or without padding.
func Base64Decode(b []byte) ([]byte, error) {
	var (
		err       error
		bts       []byte
		encodings = []*base64.Encoding{base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding, base64.StdEncoding}
	)
	for _, encoding := range encodings {
		if bts, err = encoding.DecodeString(string(b)); err == nil {
			break
		}
	}
	return bts, err
}
