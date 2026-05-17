//go:build !windows

package i18n

// Off Windows: Linux/macOS users get LC_ALL / LC_MESSAGES / LANG
// populated by their shells / login session, and detectLang already
// honors those. No platform-specific lookup needed; the default
// platformLangProvider in i18n.go returns "" already.
