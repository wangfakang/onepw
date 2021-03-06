package core

import (
	"crypto/aes"
	"crypto/md5"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mkideal/pkg/debug"
	"github.com/mkideal/pkg/textutil"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

func md5sum(i interface{}) string {
	switch v := i.(type) {
	case string:
		return fmt.Sprintf("%x", md5.Sum([]byte(v)))

	case []byte:
		return fmt.Sprintf("%x", md5.Sum(v))

	default:
		return fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%v", v))))
	}
}

// BoxRepository define repo for storing passwords
type BoxRepository interface {
	Load() ([]byte, error)
	Save([]byte) error
}

// Box represents password box
type Box struct {
	sync.RWMutex
	masterPassword string
	repo           BoxRepository
	passwords      map[string]*Password
}

// Init initialize box with master password
func (box *Box) Init(masterPassword string) error {
	//TODO: check masterPassword
	if len(masterPassword) < 6 {
		return errMasterPasswordTooShort
	}
	box.Lock()
	defer box.Unlock()
	box.masterPassword = masterPassword
	if err := box.load(); err != nil {
		return err
	}
	for _, pw := range box.passwords {
		if err := box.encrypt(pw); err != nil {
			return err
		}
	}
	return box.save()
}

// NewBox creates box with repo
func NewBox(repo BoxRepository) *Box {
	box := &Box{
		repo:      repo,
		passwords: map[string]*Password{},
	}
	return box
}

// Load loads password box
func (box *Box) Load() error {
	box.Lock()
	defer box.Unlock()
	return box.load()
}

func (box *Box) load() error {
	data, err := box.repo.Load()
	if err != nil {
		return err
	}
	return box.unmarshal(data)
}

// Save saves password box
func (box *Box) Save() error {
	box.Lock()
	defer box.Unlock()
	return box.save()
}

func (box *Box) save() error {
	data, err := box.marshal()
	if err != nil {
		return err
	}
	debug.Debugf("marshal result: %v", string(data))
	return box.repo.Save(data)
}

// Add adds a new password to box
func (box *Box) Add(pw *Password) (id string, new bool, err error) {
	debug.Debugf("Add new password: %v", pw)
	box.Lock()
	defer box.Unlock()
	if box.masterPassword == "" {
		err = errEmptyMasterPassword
		return
	}
	if old, ok := box.passwords[pw.ID]; ok {
		old.LastUpdatedAt = time.Now().Unix()
		old.migrate(pw)
		pw = old
		new = false
	} else if pw.ID != "" {
		err = newErrPasswordNotFound(pw.ID)
		return
	} else {
		id, err = box.allocID()
		if err != nil {
			return
		}
		pw.ID = id
		new = true
	}
	if err = box.encrypt(pw); err != nil {
		return
	}
	box.passwords[pw.ID] = pw
	debug.Debugf("add new password: %v", pw)
	err = box.save()
	return
}

// Remove removes passwords by ids
func (box *Box) Remove(ids []string, all bool) ([]string, error) {
	box.Lock()
	defer box.Unlock()
	if box.masterPassword == "" {
		return nil, errEmptyMasterPassword
	}
	deletedIds := []string{}
	passwords := make([]*Password, 0)

	for _, id := range ids {
		size := len(deletedIds)
		if foundPw, ok := box.passwords[id]; !ok {
			for _, pw := range box.passwords {
				if strings.HasPrefix(pw.ID, id) {
					deletedIds = append(deletedIds, pw.ID)
					passwords = append(passwords, pw)
				}
			}
		} else {
			deletedIds = append(deletedIds, id)
			passwords = append(passwords, foundPw)
		}
		if len(deletedIds) == size {
			return nil, newErrPasswordNotFound(id)
		}
		if len(deletedIds) > 1+size && !all {
			return nil, newErrAmbiguous(passwords[size:])
		}
	}
	deleted := make([]string, 0, len(deletedIds))
	for _, id := range deletedIds {
		if _, ok := box.passwords[id]; ok {
			delete(box.passwords, id)
			deleted = append(deleted, id)
		}
	}
	return deleted, box.save()
}

// RemoveByAccount removes passwords by category and account
func (box *Box) RemoveByAccount(category, account string, all bool) ([]string, error) {
	box.Lock()
	defer box.Unlock()
	if box.masterPassword == "" {
		return nil, errEmptyMasterPassword
	}
	passwords := box.find(func(pw *Password) bool {
		return pw.Category == category && pw.PlainAccount == account
	})
	if len(passwords) == 0 {
		return nil, newErrPasswordNotFoundWithAccount(category, account)
	}
	if len(passwords) > 1 && !all {
		return nil, newErrAmbiguous(passwords)
	}
	ids := []string{}
	for _, pw := range passwords {
		delete(box.passwords, pw.ID)
		ids = append(ids, pw.ID)
	}
	return ids, box.save()
}

// Clear clear password box
func (box *Box) Clear() ([]string, error) {
	box.Lock()
	defer box.Unlock()
	ids := make([]string, 0, len(box.passwords))
	for _, pw := range box.passwords {
		ids = append(ids, pw.ID)
		delete(box.passwords, pw.ID)
	}
	if len(ids) > 0 {
		return ids, box.save()
	}
	return ids, nil
}

func (box *Box) find(cond func(*Password) bool) []*Password {
	ret := []*Password{}
	for _, pw := range box.passwords {
		if cond(pw) {
			ret = append(ret, pw)
		}
	}
	return ret
}

// List writes all passwords to specified writer
func (box *Box) List(w io.Writer, noHeader bool) error {
	box.RLock()
	defer box.RUnlock()
	if box.masterPassword == "" {
		return errEmptyMasterPassword
	}
	var table textutil.Table
	table = passwordSlice(box.sortedPasswords())
	if !noHeader {
		table = textutil.AddTableHeader(table, passwordHeader)
	}
	textutil.WriteTable(w, table)
	return nil
}

// Find finds password by word
func (box *Box) Find(w io.Writer, word string) error {
	box.RLock()
	defer box.RUnlock()
	if box.masterPassword == "" {
		return errEmptyMasterPassword
	}
	table := passwordPtrSlice(box.find(func(pw *Password) bool { return pw.match(word) }))
	sort.Stable(table)
	textutil.WriteTable(w, table)
	return nil
}

func (box *Box) sortedPasswords() []Password {
	passwords := make([]Password, 0, len(box.passwords))
	for _, pw := range box.passwords {
		passwords = append(passwords, *pw)
	}
	sort.Stable(passwordSlice(passwords))
	return passwords
}

func (box *Box) allocID() (string, error) {
	count := 0
	for count < 10 {
		id := md5sum(rand.Int63())
		if _, ok := box.passwords[id]; !ok {
			return id, nil
		}
	}
	return "", errAllocateID
}

func (box *Box) marshal() ([]byte, error) {
	for _, pw := range box.passwords {
		if err := box.encrypt(pw); err != nil {
			return nil, err
		}
	}
	passwords := box.sortedPasswords()
	return json.MarshalIndent(passwords, "", "    ")
}

func (box *Box) unmarshal(data []byte) error {
	if data == nil || len(data) == 0 {
		return nil
	}
	passwords := make([]Password, 0)
	err := json.Unmarshal(data, &passwords)
	if err != nil {
		return err
	}
	debug.Debugf("unmarshal result: %v", passwords)

	for i := range passwords {
		pw := &(passwords[i])
		if box.masterPassword != "" {
			if err := box.decrypt(pw); err != nil {
				return err
			}
		}
		box.passwords[pw.ID] = pw
	}
	debug.Debugf("load result: %v", box.passwords)
	return nil
}

func (box *Box) encrypt(pw *Password) error {
	block, err := aes.NewCipher([]byte(md5sum(box.masterPassword)))
	if err != nil {
		return err
	}
	if len(pw.AccountIV) != block.BlockSize() {
		pw.AccountIV = make([]byte, block.BlockSize())
		if _, err := crand.Read(pw.AccountIV); err != nil {
			return err
		}
	}
	if len(pw.PasswordIV) != block.BlockSize() {
		pw.PasswordIV = make([]byte, block.BlockSize())
		if _, err := crand.Read(pw.PasswordIV); err != nil {
			return err
		}
	}
	pw.CipherAccount = cfbEncrypt(block, pw.AccountIV, []byte(pw.PlainAccount))
	pw.CipherPassword = cfbEncrypt(block, pw.PasswordIV, []byte(pw.PlainPassword))
	return nil
}

func (box *Box) decrypt(pw *Password) error {
	block, err := aes.NewCipher([]byte(md5sum(box.masterPassword)))
	if err != nil {
		return err
	}
	if len(pw.AccountIV) != block.BlockSize() {
		return errLengthOfIV
	}
	if len(pw.PasswordIV) != block.BlockSize() {
		return errLengthOfIV
	}
	pw.PlainAccount = string(cfbDecrypt(block, pw.AccountIV, pw.CipherAccount))
	pw.PlainPassword = string(cfbDecrypt(block, pw.PasswordIV, pw.CipherPassword))
	return nil
}

// sort passwords by Id
type passwordSlice []Password

func (ps passwordSlice) Len() int           { return len(ps) }
func (ps passwordSlice) Less(i, j int) bool { return ps[i].ID < ps[j].ID }
func (ps passwordSlice) Swap(i, j int)      { ps[i], ps[j] = ps[j], ps[i] }
func (ps passwordSlice) RowCount() int      { return ps.Len() }
func (ps passwordSlice) ColCount() int {
	if ps.Len() == 0 {
		return 0
	}
	return ps[0].colCount()
}
func (ps passwordSlice) Get(i, j int) string {
	return ps[i].get(j)
}

type passwordPtrSlice []*Password

func (ps passwordPtrSlice) Len() int           { return len(ps) }
func (ps passwordPtrSlice) Less(i, j int) bool { return ps[i].ID < ps[j].ID }
func (ps passwordPtrSlice) Swap(i, j int)      { ps[i], ps[j] = ps[j], ps[i] }
func (ps passwordPtrSlice) RowCount() int      { return ps.Len() }
func (ps passwordPtrSlice) ColCount() int {
	if ps.Len() == 0 {
		return 0
	}
	return ps[0].colCount()
}
func (ps passwordPtrSlice) Get(i, j int) string {
	return ps[i].get(j)
}
