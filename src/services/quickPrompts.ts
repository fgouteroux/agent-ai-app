import { config } from '@grafana/runtime';
import { getLandingText, type ResponseLanguage } from './landingText';

const STORAGE_KEY_PREFIX = 'agent_ai_quick_prompts_';

export interface QuickPrompt {
    id: string;
    title: string;
    content: string;
}

// Defaults are aimed at someone who has no idea what to ask first
// ("introduction"), someone chasing a live problem ("incidents"), and a
// generic topology question ("information") -- generic on purpose, since
// this plugin makes no assumption about the Grafana instance it's installed on.
//
// Each is deliberately scoped to a single tool call. A first-time user has
// no saved conversation yet, so these run against the full system prompt
// with zero compaction headroom -- a broad "investigate everything" ask
// here (multiple tool categories chained in one turn) is exactly the
// worst-case shape for free-tier LLM rate limits (e.g. Groq's ~12k TPM),
// and would be the very first thing a new user sees fail.
export function defaultQuickPrompts(language: ResponseLanguage): QuickPrompt[] {
    const t = getLandingText(language).quickPrompts;
    return [
        { id: 'introduction', title: t.introduction.title, content: t.introduction.content },
        { id: 'incidents', title: t.incidents.title, content: t.incidents.content },
        { id: 'information', title: t.information.title, content: t.information.content },
    ];
}

function storageKey(): string {
    const userId = config.bootData?.user?.id ?? 'anonymous';
    return `${STORAGE_KEY_PREFIX}${userId}`;
}

export function getQuickPrompts(language: ResponseLanguage): QuickPrompt[] {
    const defaults = defaultQuickPrompts(language);
    try {
        const stored = localStorage.getItem(storageKey());
        if (!stored) {
            return defaults;
        }
        const parsed = JSON.parse(stored);
        if (!Array.isArray(parsed) || parsed.length !== defaults.length) {
            return defaults;
        }
        return parsed;
    } catch (e) {
        console.error('[AgentAI] Error loading quick prompts:', e);
        return defaults;
    }
}

export function saveQuickPrompt(id: string, title: string, content: string, language: ResponseLanguage): QuickPrompt[] {
    const current = getQuickPrompts(language);
    const updated = current.map((p) => (p.id === id ? { ...p, title, content } : p));
    try {
        localStorage.setItem(storageKey(), JSON.stringify(updated));
    } catch (e) {
        console.error('[AgentAI] Error saving quick prompts:', e);
    }
    return updated;
}
