package transcribe

import "strings"

// languageNames maps the English (and a few native) names returned by
// Whisper-style APIs to ISO 639-1 codes.
var languageNames = map[string]string{
	"afrikaans": "af", "arabic": "ar", "armenian": "hy", "azerbaijani": "az",
	"belarusian": "be", "bosnian": "bs", "bulgarian": "bg", "catalan": "ca",
	"chinese": "zh", "croatian": "hr", "czech": "cs", "danish": "da",
	"dutch": "nl", "english": "en", "estonian": "et", "finnish": "fi",
	"french": "fr", "galician": "gl", "german": "de", "greek": "el",
	"hebrew": "he", "hindi": "hi", "hungarian": "hu", "icelandic": "is",
	"indonesian": "id", "italian": "it", "japanese": "ja", "kannada": "kn",
	"kazakh": "kk", "korean": "ko", "latvian": "lv", "lithuanian": "lt",
	"macedonian": "mk", "malay": "ms", "marathi": "mr", "maori": "mi",
	"nepali": "ne", "norwegian": "no", "persian": "fa", "polish": "pl",
	"portuguese": "pt", "romanian": "ro", "russian": "ru", "serbian": "sr",
	"slovak": "sk", "slovenian": "sl", "spanish": "es", "swahili": "sw",
	"swedish": "sv", "tagalog": "tl", "tamil": "ta", "thai": "th",
	"turkish": "tr", "ukrainian": "uk", "urdu": "ur", "vietnamese": "vi",
	"welsh": "cy", "albanian": "sq", "basque": "eu", "bengali": "bn",
	"italiano": "it", "inglese": "en", "tedesco": "de", "francese": "fr",
	"spagnolo": "es", "portoghese": "pt", "russo": "ru",
}

// languageLabels gives the target name used in translation prompts.
var languageLabels = map[string]string{}

func init() {
	for name, code := range languageNames {
		if _, ok := languageLabels[code]; !ok || isEnglishName(name) {
			languageLabels[code] = name
		}
	}
}

func isEnglishName(name string) bool {
	switch name {
	case "italiano", "inglese", "tedesco", "francese", "spagnolo", "portoghese", "russo":
		return false
	}
	return true
}

// NormalizeLanguage turns "it", "IT", "it-IT", "Italian" into "it". Unknown
// names give "" (unknown) rather than a wrong code.
func NormalizeLanguage(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	if i := strings.IndexAny(s, "-_"); i > 0 {
		s = s[:i]
	}
	if len(s) == 2 {
		return s
	}
	return languageNames[s]
}

// languageLabel is "Italian (it)" for prompts.
func languageLabel(code string) string {
	if name, ok := languageLabels[code]; ok {
		return strings.ToUpper(name[:1]) + name[1:] + " (" + code + ")"
	}
	return code
}
