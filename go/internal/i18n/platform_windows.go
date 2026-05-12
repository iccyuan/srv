//go:build windows

package i18n

import (
	"strings"
	"syscall"
	"unsafe"
)

// LOCALE_NAME_MAX_LENGTH from winnls.h.
const localeNameMaxLength = 85

var procGetUserDefaultLocaleName = syscall.NewLazyDLL("kernel32.dll").
	NewProc("GetUserDefaultLocaleName")

// platformLang returns the user's regional locale as a lowercase
// BCP-47 tag (e.g. "zh-cn") via the Win32 GetUserDefaultLocaleName.
// Empty string when the lookup fails.
//
// detectLang only consults this when none of LC_ALL / LC_MESSAGES /
// LANG are set -- i.e., the typical Windows case. On a localized
// Windows install (中文 / 日本語 / etc.) those POSIX envs are empty,
// so without this fallback srv would always pick English even when
// the user expects their system language.
//
// We use the *regional* locale (Get-Culture in PowerShell terms),
// not GetUserDefaultUILanguage. Region tracks "what language does
// the user think of themselves as using" more reliably than the
// Windows display language, which is often left at English even by
// non-English speakers.
func platformLang() string {
	var buf [localeNameMaxLength]uint16
	r, _, _ := procGetUserDefaultLocaleName.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(localeNameMaxLength),
	)
	if r == 0 {
		return ""
	}
	return strings.ToLower(syscall.UTF16ToString(buf[:]))
}

func init() {
	platformLangProvider = platformLang
}
