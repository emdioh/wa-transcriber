package bot

import (
	"fmt"

	"github.com/emdioh/wa-transcriber/internal/config"
)

// msgKey identifies one bot reply. Every user-visible string lives in the
// messages table below; TestMessagesComplete fails if a language misses one.
type msgKey int

const (
	msgReplyToVoice msgKey = iota
	msgTranscriptionUsage
	msgHelpAdmin
	msgTranscriptionState
	msgAutoOnChat
	msgAutoOffChat
	msgAutoOnGlobal
	msgAutoOffGlobal
	msgInvalidArgs
	msgSaveFailed
	msgResetPrompt
	msgResetUsage
	msgResetNothing
	msgResetDone
	msgPasswordTooShort
	msgPasswordUsage
	msgPasswordSaveFailed
	msgPasswordGenerated
	msgPasswordUpdated
	msgTranscriptionNotConfigured
	msgTranscriptionFailed
	msgNoSpeech
	msgOn
	msgOff
	msgStatusHeader
	msgStatusWhatsApp
	msgStatusOnline
	msgStatusOnlineAs
	msgStatusOffline
	msgStatusLastMessage
	msgStatusLastTranscription
	msgStatusTranscription
	msgStatusNotConfigured
	msgNotSet
	msgStatusUptime
	msgStatusCoordination
	msgModeClaim
	msgModeObserve
	msgStatusClock
	msgStatusInterfaces
	msgStatusWebDisabled
	msgStatusWebHeader
	msgStatusNoAddress
	msgNever
	msgAgo
	msgUpdateHeader
	msgUpdateLine
	msgUpdateCheck
	msgUpdateAuto
	msgUpdateFound
	msgUpdateNone
	msgUpdateFailed
	msgUpdateRollback
	msgUpdateUnavailable
	msgUpdateDisabled
	msgKeyCount
)

// messages holds every reply, per language. Format verbs must match the
// arguments at the call site; missing entries fall back to English.
var messages = map[string]map[msgKey]string{
	"en": {
		msgReplyToVoice: "ℹ️ Reply to a voice note with !t to transcribe it.",
		msgTranscriptionUsage: "ℹ️ Commands:\n" +
			"• !t (reply to a voice note): transcribe it\n" +
			"• !transcription: show the state\n" +
			"• !transcription on|off: auto-transcription in this chat\n" +
			"• !transcription global on|off: default for all chats",
		msgHelpAdmin: "Only in this chat:\n" +
			"• !status: device status\n" +
			"• !password [new]: set the web UI password (random if omitted)\n" +
			"• !reset: unlink WhatsApp and erase everything",
		msgTranscriptionState: "ℹ️ Auto-transcription\n• Global: %s\n• This chat: %s\n• Effective: %s\n\n%s",
		msgAutoOnChat:         "✅ Auto-transcription on in this chat.",
		msgAutoOffChat:        "✅ Auto-transcription off in this chat.",
		msgAutoOnGlobal:       "✅ Auto-transcription on by default.",
		msgAutoOffGlobal:      "✅ Auto-transcription off by default.",
		msgInvalidArgs:        "⚠️ Invalid arguments.\n\n%s",
		msgSaveFailed:         "⚠️ Could not save the setting.",
		msgResetPrompt: "⚠️ This unlinks WhatsApp and erases all settings, the web password " +
			"and the WiFi networks.\nSend \"!reset confirm\" within 60 seconds to proceed.",
		msgResetUsage:   "ℹ️ Usage: !reset, then !reset confirm",
		msgResetNothing: "ℹ️ Nothing to confirm: send !reset first.",
		msgResetDone: "♻️ Resetting. The device restarts in setup mode; this chat will " +
			"no longer receive replies.",
		msgPasswordTooShort:           "⚠️ The password needs at least %d characters.",
		msgPasswordUsage:              "ℹ️ Usage: !password [new password, no spaces]",
		msgPasswordSaveFailed:         "⚠️ Could not save the password.",
		msgPasswordGenerated:          "🔑 New web UI password: %s",
		msgPasswordUpdated:            "🔑 Web UI password updated.",
		msgTranscriptionNotConfigured: "⚠️ Transcription is not configured yet (open the web UI).",
		msgTranscriptionFailed:        "⚠️ Transcription failed, try again later with !t.",
		msgNoSpeech:                   "(no speech recognised)",
		msgOn:                         "on",
		msgOff:                        "off",
		msgStatusHeader:               "📟 wa-transcriber %s\n",
		msgStatusWhatsApp:             "• WhatsApp: %s\n",
		msgStatusOnline:               "online",
		msgStatusOnlineAs:             "online as +%s",
		msgStatusOffline:              "offline",
		msgStatusLastMessage:          "• Last message seen: %s\n",
		msgStatusLastTranscription:    "• Last transcription: %s\n",
		msgStatusTranscription:        "• Transcription: %s, global %s, %d chat override(s)\n",
		msgStatusNotConfigured:        "not configured",
		msgNotSet:                     "not set",
		msgStatusUptime:               "• Uptime: %s\n",
		msgStatusCoordination:         "• Coordination: %s, claims %d, stood down %d, took over %d\n",
		msgModeClaim:                  "claim",
		msgModeObserve:                "observe-only",
		msgStatusClock:                "• Clock: ⚠ not synced when WhatsApp was connected\n",
		msgStatusInterfaces:           "• Interfaces: %s\n",
		msgStatusWebDisabled:          "• Web UI: disabled",
		msgStatusWebHeader:            "• Web UI:\n%s",
		msgStatusNoAddress:            "no network address",
		msgNever:                      "never",
		msgAgo:                        "%s (%s ago)",
		msgUpdateHeader:               "⬆️ Software updates\n",
		msgUpdateLine:                 "• Version: %s\n",
		msgUpdateCheck:                "• Automatic check: %s\n",
		msgUpdateAuto:                 "• Install without asking: %s\n",
		msgUpdateFound:                "• Available: %s (%s)\n",
		msgUpdateNone:                 "• Available: no new version\n",
		msgUpdateFailed:               "• Last attempt: ⚠ %s\n",
		msgUpdateRollback:             "• Previous version: %s\n",
		msgUpdateUnavailable:          "⚠️ Updates are not available on this device (open the web UI).",
		msgUpdateDisabled:             "ℹ️ Updates are off on this device.",
	},
	"it": {
		msgReplyToVoice: "ℹ️ Rispondi a un vocale con !t per trascriverlo.",
		msgTranscriptionUsage: "ℹ️ Comandi:\n" +
			"• !t (rispondi a un vocale): trascrivilo\n" +
			"• !transcription: mostra lo stato\n" +
			"• !transcription on|off: trascrizione automatica in questa chat\n" +
			"• !transcription global on|off: impostazione predefinita per tutte le chat",
		msgHelpAdmin: "Solo in questa chat:\n" +
			"• !status: stato del dispositivo\n" +
			"• !password [nuova]: imposta la password della web UI (casuale se omessa)\n" +
			"• !reset: scollega WhatsApp e cancella tutto",
		msgTranscriptionState: "ℹ️ Trascrizione automatica\n• Globale: %s\n• Questa chat: %s\n• In vigore: %s\n\n%s",
		msgAutoOnChat:         "✅ Trascrizione automatica attivata in questa chat.",
		msgAutoOffChat:        "✅ Trascrizione automatica disattivata in questa chat.",
		msgAutoOnGlobal:       "✅ Trascrizione automatica attivata per impostazione predefinita.",
		msgAutoOffGlobal:      "✅ Trascrizione automatica disattivata per impostazione predefinita.",
		msgInvalidArgs:        "⚠️ Argomenti non validi.\n\n%s",
		msgSaveFailed:         "⚠️ Impossibile salvare l'impostazione.",
		msgResetPrompt: "⚠️ Questo scollega WhatsApp e cancella tutte le impostazioni, la password " +
			"della web UI e le reti WiFi.\nInvia \"!reset confirm\" entro 60 secondi per procedere.",
		msgResetUsage:   "ℹ️ Uso: !reset, poi !reset confirm",
		msgResetNothing: "ℹ️ Niente da confermare: invia prima !reset.",
		msgResetDone: "♻️ Ripristino in corso. Il dispositivo si riavvia in modalità " +
			"configurazione; questa chat non riceverà più risposte.",
		msgPasswordTooShort:           "⚠️ La password deve avere almeno %d caratteri.",
		msgPasswordUsage:              "ℹ️ Uso: !password [nuova password, senza spazi]",
		msgPasswordSaveFailed:         "⚠️ Impossibile salvare la password.",
		msgPasswordGenerated:          "🔑 Nuova password della web UI: %s",
		msgPasswordUpdated:            "🔑 Password della web UI aggiornata.",
		msgTranscriptionNotConfigured: "⚠️ La trascrizione non è ancora configurata (apri la web UI).",
		msgTranscriptionFailed:        "⚠️ Trascrizione non riuscita, riprova più tardi con !t.",
		msgNoSpeech:                   "(nessun parlato riconosciuto)",
		msgOn:                         "attiva",
		msgOff:                        "disattiva",
		msgStatusHeader:               "📟 wa-transcriber %s\n",
		msgStatusWhatsApp:             "• WhatsApp: %s\n",
		msgStatusOnline:               "online",
		msgStatusOnlineAs:             "online come +%s",
		msgStatusOffline:              "offline",
		msgStatusLastMessage:          "• Ultimo messaggio visto: %s\n",
		msgStatusLastTranscription:    "• Ultima trascrizione: %s\n",
		msgStatusTranscription:        "• Trascrizione: %s, globale %s, %d chat con impostazione propria\n",
		msgStatusNotConfigured:        "non configurata",
		msgNotSet:                     "non impostata",
		msgStatusUptime:               "• Attivo da: %s\n",
		msgStatusCoordination:         "• Coordinamento: %s, richieste %d, rinunce %d, sostituzioni %d\n",
		msgModeClaim:                  "claim",
		msgModeObserve:                "solo osservazione",
		msgStatusClock:                "• Orologio: ⚠ non sincronizzato quando WhatsApp si è collegato\n",
		msgStatusInterfaces:           "• Interfacce: %s\n",
		msgStatusWebDisabled:          "• Web UI: disattivata",
		msgStatusWebHeader:            "• Web UI:\n%s",
		msgStatusNoAddress:            "nessun indirizzo di rete",
		msgNever:                      "mai",
		msgAgo:                        "%s (%s fa)",
		msgUpdateHeader:               "⬆️ Aggiornamenti del software\n",
		msgUpdateLine:                 "• Versione: %s\n",
		msgUpdateCheck:                "• Controllo automatico: %s\n",
		msgUpdateAuto:                 "• Installa senza chiedere: %s\n",
		msgUpdateFound:                "• Disponibile: %s (%s)\n",
		msgUpdateNone:                 "• Disponibile: nessuna nuova versione\n",
		msgUpdateFailed:               "• Ultimo tentativo: ⚠ %s\n",
		msgUpdateRollback:             "• Versione precedente: %s\n",
		msgUpdateUnavailable:          "⚠️ Gli aggiornamenti non sono disponibili su questo dispositivo (apri la web UI).",
		msgUpdateDisabled:             "ℹ️ Gli aggiornamenti sono disattivati su questo dispositivo.",
	},
}

// language is the configured reply language, English when unset or unknown.
func (b *Bot) language() string {
	if b.Config != nil {
		return config.NormalizeLanguage(b.Config.Get().Language)
	}
	return config.DefaultLanguage
}

// t returns the reply for key in the configured language, formatted with args.
func (b *Bot) t(key msgKey, args ...any) string {
	text, ok := messages[b.language()][key]
	if !ok {
		text = messages[config.DefaultLanguage][key]
	}
	if len(args) == 0 {
		return text
	}
	return fmt.Sprintf(text, args...)
}

// helpText is the !help reply: the command list plus the admin-only part.
func (b *Bot) helpText() string {
	return b.t(msgTranscriptionUsage) + "\n\n" + b.t(msgHelpAdmin)
}

// onoff renders a boolean for the configured language.
func (b *Bot) onoff(v bool) string {
	if v {
		return b.t(msgOn)
	}
	return b.t(msgOff)
}
