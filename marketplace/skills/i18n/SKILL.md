---
name: i18n
description: Internationalization: extract strings, translations.
---

# Internationalization

Make user-facing text translatable and keep translations complete. Use the i18n library the project already has; only introduce one if there is none, and ask first.

## 1. Survey

- Find the framework's i18n mechanism (i18next, react-intl, vue-i18n, gettext, go-i18n, .resx, Android strings.xml…) and where locale files live.
- List supported locales and the default/fallback locale.

## 2. Extract strings

- Move every user-visible literal into the message catalog: UI text, errors shown to users, emails, notifications.
- Leave log messages, identifiers, and protocol values untranslated.
- Keys are stable and descriptive (`checkout.payment.cardDeclined`), not the English text.
- Never build sentences by concatenation; use placeholders so translators can reorder: `"{count} files deleted from {folder}"`.
- Use the library's plural and select forms (ICU MessageFormat or equivalent) instead of `if count == 1`.

## 3. Formatting

- Dates, times, numbers, currencies and lists go through locale-aware formatters (`Intl.*`, CLDR-based APIs), never hand-written formats.
- Store and transmit times in UTC; convert at display.
- Check layouts for longer text (German) and right-to-left scripts (Arabic, Hebrew) if those locales are supported.

## 4. Translations

- Add every new key to all locale files. When you cannot translate reliably, copy the source text and mark it for review in the way the project already does.
- Keep placeholders identical across locales.

## 5. Verify

- Run the project's extraction/lint command if it has one, or write a quick check that every locale has the same key set and matching placeholders.
- Run the app or tests in a non-default locale and look for untranslated literals.
- Report added keys, missing translations, and strings you deliberately left untranslated.
