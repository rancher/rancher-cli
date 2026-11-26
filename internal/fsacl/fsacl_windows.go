//go:build windows

package fsacl

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"golang.org/x/sys/windows"
)

// Harden replaces path's DACL with a single owner-only (current user, full
// control) entry, with inheritance BLOCKED
// (PROTECTED_DACL_SECURITY_INFORMATION): without the PROTECTED flag the
// parent directory's inherited ACEs would remain and the restriction
// would be cosmetic. It is for a path that already exists. A file that
// another user can reach through an inherited entry of its directory is open
// to that user between its creation and this call, and a handle opened in
// that time keeps its access afterwards: create such a file with CreateFile
// or CreateTemp, which set the same list in the call that creates it. This
// is the Windows counterpart of the Unix 0600 chmod, which NTFS ignores.
func Harden(path string) error {
	return set(path, windows.NO_INHERITANCE)
}

// HardenInheritable is Harden for a directory whose entries should carry
// the same restriction: the single owner-only entry is marked to pass to
// files and subdirectories created in the directory afterwards; a
// caller sets it once on the directory and creates files in it with no
// further call. The same exposure applies to the directory itself between
// its creation and this call; Mkdir creates it with the list.
func HardenInheritable(dir string) error {
	return set(dir, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
}

// ownerOnlyACL returns an access list with one entry that grants the
// current user full control, with the inheritance flags given.
func ownerOnlyACL(inheritance uint32) (*windows.ACL, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	ea := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}
	return windows.ACLFromEntries(ea, nil)
}

func set(path string, inheritance uint32) error {
	dacl, err := ownerOnlyACL(inheritance)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	)
}

// ownerOnlyDescriptor returns a security descriptor whose DACL is the list
// ownerOnlyACL builds, marked protected so the parent directory's entries do
// not apply to the new object.
func ownerOnlyDescriptor(inheritance uint32) (*windows.SECURITY_DESCRIPTOR, error) {
	dacl, err := ownerOnlyACL(inheritance)
	if err != nil {
		return nil, err
	}
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	if err := sd.SetDACL(dacl, true, false); err != nil {
		return nil, err
	}
	if err := sd.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return nil, err
	}
	return sd, nil
}

// ntCreate describes one create relative to a directory handle: the access
// the returned handle has, the file attributes, the create options and the
// inheritance flags of the owner-only entry.
type ntCreate struct {
	access      uint32
	attributes  uint32
	options     uint32
	inheritance uint32
}

// create runs NtCreateFile for name relative to a handle of the directory of
// root, with disposition FILE_CREATE and the owner-only security descriptor.
// It follows os.Root's own create on Windows (the code os.Root.OpenFile
// runs): the directory is named by a handle, not by a path, so a link that
// replaces a component of the path after the root was opened cannot
// redirect the create, and FILE_OPEN_REPARSE_POINT keeps a link at name
// from being followed. The one addition is the security descriptor.
func (c ntCreate) create(root *os.Root, name string) (windows.Handle, error) {
	if !validWindowsName(name) {
		return 0, invalidNameError{}
	}
	sd, err := ownerOnlyDescriptor(c.inheritance)
	if err != nil {
		return 0, fmt.Errorf("cannot build the owner-only access list: %w", err)
	}
	dir, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	defer func() { _ = dir.Close() }()

	oa := &windows.OBJECT_ATTRIBUTES{
		RootDirectory:      windows.Handle(dir.Fd()),
		Attributes:         windows.OBJ_CASE_INSENSITIVE,
		SecurityDescriptor: sd,
	}
	oa.Length = uint32(reflect.TypeFor[windows.OBJECT_ATTRIBUTES]().Size())
	if oa.ObjectName, err = windows.NewNTUnicodeString(name); err != nil {
		return 0, err
	}
	var (
		h    windows.Handle
		iosb windows.IO_STATUS_BLOCK
	)
	err = windows.NtCreateFile(&h, c.access, oa, &iosb, nil, c.attributes,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_CREATE, c.options, 0, 0)
	// The directory handle must stay open until the call returns: dir.Fd()
	// is a number, which keeps nothing alive.
	runtime.KeepAlive(dir)
	if err != nil {
		return 0, ntError(err)
	}
	if afterCreateHook != nil {
		afterCreateHook(h)
	}
	return h, nil
}

// afterCreateHook is called with the handle of a new file or directory as soon
// as NtCreateFile has returned it, before anything else uses it. A test reads
// the access list through the handle there, which is the only point at which
// a create that sets the list after the object exists can be told from one
// that creates it with the list. Nil in production.
var afterCreateHook func(h windows.Handle)

// invalidNameError is the error for a name validWindowsName refuses. It
// says what makes a name invalid, which the system's own text for
// ERROR_INVALID_NAME does not, and it matches windows.ERROR_INVALID_NAME.
type invalidNameError struct{}

func (invalidNameError) Error() string {
	return "the name ends in a dot or a space, has a colon, or is a device name such as NUL"
}

func (invalidNameError) Is(target error) bool { return target == windows.ERROR_INVALID_NAME }

// validWindowsName reports whether name is a name NtCreateFile creates as
// written. Win32 paths, which os.Stat and the other os functions use, drop a
// trailing dot or space and treat a device name such as NUL or CON as a
// device, and os.Root cleans a name the same way before every call of its
// own. A name created as written would differ from the name the same string
// reaches through those functions, so a name that those rules would change
// is refused, as is a colon, which names a stream of a file, not a file.
func validWindowsName(name string) bool {
	if name == "" || strings.ContainsRune(name, ':') {
		return false
	}
	if last := name[len(name)-1]; last == '.' || last == ' ' {
		return false
	}
	return filepath.IsLocal(name)
}

// ntError turns the status NtCreateFile returned into an error that the os
// package's predicates understand: a name that exists matches fs.ErrExist.
func ntError(err error) error {
	status, ok := errors.AsType[windows.NTStatus](err)
	if !ok {
		return err
	}
	if status == windows.STATUS_OBJECT_NAME_COLLISION {
		return windows.ERROR_ALREADY_EXISTS
	}
	return status.Errno()
}

func createFile(root *os.Root, name string, perm fs.FileMode) (*os.File, error) {
	attributes := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if perm&0o200 == 0 {
		attributes = windows.FILE_ATTRIBUTE_READONLY
	}
	h, err := ntCreate{
		access:      windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE,
		attributes:  attributes,
		options:     windows.FILE_OPEN_FOR_BACKUP_INTENT | windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_NON_DIRECTORY_FILE | windows.FILE_OPEN_REPARSE_POINT,
		inheritance: windows.NO_INHERITANCE,
	}.create(root, name)
	if err != nil {
		return nil, newPathError("openat", name, err)
	}
	return os.NewFile(uintptr(h), joinPath(root.Name(), name)), nil
}

func mkdir(root *os.Root, name string, _ fs.FileMode, inheritable bool) error {
	inheritance := uint32(windows.NO_INHERITANCE)
	if inheritable {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	h, err := ntCreate{
		access:      windows.FILE_GENERIC_READ,
		attributes:  windows.FILE_ATTRIBUTE_NORMAL,
		options:     windows.FILE_DIRECTORY_FILE | windows.FILE_OPEN_REPARSE_POINT,
		inheritance: inheritance,
	}.create(root, name)
	if err != nil {
		return newPathError("mkdir", filepath.Join(root.Name(), name), err)
	}
	return windows.CloseHandle(h)
}
