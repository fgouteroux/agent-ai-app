// Import types from centralized location
import { config, getAppEvents } from '@grafana/runtime';
import { AppEvents } from '@grafana/data';
import type { Message } from '../types/llm.types';
import type { ChatSession } from '../types/chat.types';

// Re-export for backward compatibility
export type { ChatSession };

const STORAGE_KEY_PREFIX = 'agent_ai_chat_history_';
const DEFAULT_MAX_HISTORY = 50;
// Chat history is scoped to the logged-in Grafana user and kept for a week --
// long enough to return to a conversation a few days later, not a durable
// archive. This is the user-visible history list only; see resources.go's
// audit logging for the separate, backend-side record of exchanges.
const DEFAULT_RETENTION_DAYS = 7;
const MAX_PINNED_SESSIONS = 20;
// Security-audit finding M9: cleanupOldSessions above only bounded the
// STORAGE by session count/age, never by actual byte size -- a handful of
// long conversations (or ones with large attachments) could still blow
// past a browser's real localStorage quota (every major browser guarantees
// at least ~5MB per origin; some allow more). When that happened,
// localStorage.setItem threw QuotaExceededError, caught below and only
// ever logged to the console -- the save silently failed and whatever the
// user just did in this conversation was gone the moment they reloaded,
// with no visible sign anything went wrong. 4MB stays safely under that
// universal floor, leaving headroom for whatever else already shares the
// origin's quota.
const MAX_STORAGE_BYTES = 4 * 1024 * 1024;

function notify(message: string): void {
    getAppEvents().publish({ type: AppEvents.alertError.name, payload: [message] });
}

function byteSize(sessions: ChatSession[]): number {
    return new TextEncoder().encode(JSON.stringify(sessions)).length;
}

/**
 * Storage key scoped to the current Grafana user AND organization, so
 * sessions never leak across accounts sharing a browser, nor across two
 * organizations the same user belongs to (security-audit finding M-06 --
 * this used to be scoped by user only, so switching orgs as the same user
 * kept showing the other org's conversation history/dashboard names).
 */
function storageKey(): string {
    const userId = config.bootData?.user?.id ?? 'anonymous';
    const orgId = config.bootData?.user?.orgId ?? 'no-org';
    return `${STORAGE_KEY_PREFIX}${userId}_${orgId}`;
}

class ChatHistoryService {
    private getStoredSessions(): ChatSession[] {
        try {
            const stored = localStorage.getItem(storageKey());
            return stored ? JSON.parse(stored) : [];
        } catch (e) {
            console.error('[AgentAI] Error loading chat history:', e);
            return [];
        }
    }

    // Drops the oldest unpinned sessions, one at a time, until the whole
    // set's serialized size fits MAX_STORAGE_BYTES -- a single-session
    // eviction (like cleanupOldSessions' count-based prune) isn't precise
    // enough here, since one large conversation alone can blow the budget
    // even after every other small one is gone. Pinned sessions are never
    // dropped for size (same "the user explicitly chose to keep this"
    // contract togglePinSession already enforces via MAX_PINNED_SESSIONS)
    // -- if pinned sessions alone exceed the budget, saveSessions' own
    // QuotaExceededError handling below is the last resort.
    private enforceStorageBudget(sessions: ChatSession[]): { kept: ChatSession[]; droppedCount: number } {
        if (byteSize(sessions) <= MAX_STORAGE_BYTES) {
            return { kept: sessions, droppedCount: 0 };
        }
        const pinned = sessions.filter(s => s.isPinned);
        const unpinned = sessions.filter(s => !s.isPinned).sort((a, b) => a.updatedAt - b.updatedAt);

        let droppedCount = 0;
        while (unpinned.length > 0 && byteSize([...pinned, ...unpinned]) > MAX_STORAGE_BYTES) {
            unpinned.shift();
            droppedCount++;
        }
        return { kept: [...pinned, ...unpinned], droppedCount };
    }

    private saveSessions(sessions: ChatSession[]): void {
        const { kept, droppedCount } = this.enforceStorageBudget(sessions);
        if (droppedCount > 0) {
            notify(
                `Local chat history was too large to store -- removed ${droppedCount} oldest ` +
                `conversation${droppedCount === 1 ? '' : 's'} to make room. Pin a conversation to keep it from being removed this way.`
            );
        }
        try {
            localStorage.setItem(storageKey(), JSON.stringify(kept));
        } catch (e) {
            console.error('[AgentAI] Error saving chat history:', e);
            notify("Couldn't save this conversation to your local history -- your browser's storage is full.");
        }
    }

    private generateId(): string {
        return `chat_${Date.now()}_${Math.random().toString(36).substring(2, 9)}`;
    }

    private generateTitle(messages: Message[]): string {
        const firstUserMessage = messages.find(m => m.role === 'user');
        if (firstUserMessage) {
            const content = firstUserMessage.content.trim();
            return content.length > 50 ? content.substring(0, 47) + '...' : content;
        }
        return 'New Chat';
    }

    getAllSessions(): ChatSession[] {
        this.cleanupOldSessions();
        return this.getStoredSessions().sort((a, b) => {
            // Pinned sessions come first
            if (a.isPinned && !b.isPinned) {
                return -1;
            }
            if (!a.isPinned && b.isPinned) {
                return 1;
            }
            // Then sort by updatedAt (newest first)
            return b.updatedAt - a.updatedAt;
        });
    }

    getSession(id: string): ChatSession | undefined {
        return this.getStoredSessions().find(s => s.id === id);
    }

    togglePinSession(id: string): boolean {
        const sessions = this.getStoredSessions();
        const session = sessions.find(s => s.id === id);

        if (!session) {
            return false;
        }

        if (!session.isPinned) {
            // Check limit before pinning
            const pinnedCount = sessions.filter(s => s.isPinned).length;
            if (pinnedCount >= MAX_PINNED_SESSIONS) {
                return false;
            }
            session.isPinned = true;
        } else {
            session.isPinned = false;
        }

        this.saveSessions(sessions);
        return true;
    }

    saveSession(messages: Message[], sessionId?: string, agent?: string, agentLabel?: string): ChatSession {
        const sessions = this.getStoredSessions();
        const now = Date.now();

        let session: ChatSession;

        if (sessionId) {
            const existing = sessions.find(s => s.id === sessionId);
            if (existing) {
                existing.messages = messages;
                existing.updatedAt = now;
                existing.title = this.generateTitle(messages);
                existing.agent = agent;
                existing.agentLabel = agentLabel;
                session = existing;
            } else {
                session = {
                    id: sessionId,
                    title: this.generateTitle(messages),
                    messages,
                    createdAt: now,
                    updatedAt: now,
                    agent,
                    agentLabel,
                };
                sessions.push(session);
            }
        } else {
            session = {
                id: this.generateId(),
                title: this.generateTitle(messages),
                messages,
                createdAt: now,
                updatedAt: now,
                agent,
                agentLabel,
            };
            sessions.push(session);
        }

        this.saveSessions(sessions);
        return session;
    }

    deleteSession(id: string): void {
        const sessions = this.getStoredSessions().filter(s => s.id !== id);
        this.saveSessions(sessions);
    }

    cleanupOldSessions(maxHistory: number = DEFAULT_MAX_HISTORY, retentionDays: number = DEFAULT_RETENTION_DAYS): void {
        let sessions = this.getStoredSessions();

        // Remove sessions older than retention period, but keep pinned ones
        const cutoffTime = Date.now() - (retentionDays * 24 * 60 * 60 * 1000);
        sessions = sessions.filter(s => s.isPinned || s.createdAt > cutoffTime);

        // Keep only the most recent sessions up to maxHistory, but always keep pinned
        // ones -- availableSlots below is clamped to 0, so pinned sessions exceeding
        // maxHistory on their own just means zero unpinned sessions survive, not a
        // negative slice or an error.
        const pinnedSessions = sessions.filter(s => s.isPinned);
        let unpinnedSessions = sessions.filter(s => !s.isPinned);

        if (unpinnedSessions.length + pinnedSessions.length > maxHistory) {
            const availableSlots = Math.max(0, maxHistory - pinnedSessions.length);
            unpinnedSessions = unpinnedSessions.sort((a, b) => b.updatedAt - a.updatedAt).slice(0, availableSlots);
        }

        sessions = [...pinnedSessions, ...unpinnedSessions];

        this.saveSessions(sessions);
    }

    clearAll(): void {
        localStorage.removeItem(storageKey());
    }
}

export const chatHistoryService = new ChatHistoryService();
