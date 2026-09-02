import type { PluginExtensionPanelContext } from '@grafana/data';

import { summarizePanelData } from './panelData';

/**
 * Hands a panel over to the chat page opened in a NEW BROWSER TAB.
 *
 * The docked side panel gets its context by simply being handed the live
 * object; a new tab cannot. A URL is the obvious channel and the wrong one
 * here: the interesting part is the panel's downsampled data, several
 * kilobytes of it, which does not belong in a query string (length limits,
 * server logs, a link nobody can read or share). So the URL carries an
 * opaque id and the payload goes through localStorage, which the new tab
 * reads on mount.
 *
 * localStorage rather than sessionStorage: a tab opened with `noopener` --
 * which is the right way to open one -- gets no copy of the session store.
 * Entries are pruned by age instead of deleted on read, so that reloading
 * the chat tab keeps working; nothing here is secret beyond what the user is
 * already looking at, and it never leaves the browser.
 */

const KEY_PREFIX = 'agent_ai_panel_handoff:';

/** Long enough to survive opening the tab and a reload or two, short enough
 *  that a stale panel never resurfaces days later in an unrelated chat. */
const MAX_AGE_MS = 30 * 60 * 1000;

export interface PanelHandoff {
    title: string;
    /** Panel id within the dashboard -- what tells get_dashboard's output
     *  which of its panels this conversation started from. */
    panelId?: number;
    dashboardTitle?: string;
    /** The dashboard's UID, so a follow-up about ANOTHER panel of the same
     *  dashboard ("and the one next to it?") is one get_dashboard call away
     *  instead of a search the model has to guess its way through. */
    dashboardUid?: string;
    queries?: string[];
    timeRange: { from: string; to: string };
    /** UIDs only -- the receiving tab resolves names and types itself, from
     *  its own datasource list, rather than trusting a stored copy. */
    datasourceUids?: string[];
    /** Pre-formatted by summarizePanelData: the values the panel was showing
     *  when the tab was opened. The new tab has no frames of its own -- it
     *  never ran the query -- so this is the only way the model gets to read
     *  what the user was actually looking at. */
    displayedData?: string;
    createdAt: number;
}

function safe<T>(read: () => T, fallback: T): T {
    try {
        return read();
    } catch {
        return fallback;
    }
}

/** Drops handoffs older than MAX_AGE_MS. Called on write, so the store stays
 *  bounded without needing anything to run in the background. */
function prune(now: number): void {
    safe(() => {
        const stale: string[] = [];
        for (let i = 0; i < window.localStorage.length; i++) {
            const key = window.localStorage.key(i);
            if (!key?.startsWith(KEY_PREFIX)) {
                continue;
            }
            const createdAt = safe(() => JSON.parse(window.localStorage.getItem(key) ?? '{}').createdAt, 0);
            if (typeof createdAt !== 'number' || now - createdAt > MAX_AGE_MS) {
                stale.push(key);
            }
        }
        stale.forEach((key) => window.localStorage.removeItem(key));
    }, undefined);
}

/**
 * Reads everything worth carrying from the live panel context.
 *
 * Every read goes through safe(): this object is Grafana's read-only proxy,
 * the same one that throws on properties whose getter caches into the field
 * (see panelData.ts). A panel we cannot fully describe still opens a chat.
 */
export function panelHandoffFromContext(panelContext: Readonly<PluginExtensionPanelContext>): PanelHandoff {
    return {
        title: safe(() => panelContext.title, 'panel'),
        panelId: safe(() => panelContext.id, undefined),
        dashboardTitle: safe(() => panelContext.dashboard?.title, undefined),
        dashboardUid: safe(() => (panelContext.dashboard as { uid?: string } | undefined)?.uid, undefined),
        queries: safe(
            () =>
                (panelContext.targets ?? [])
                    .map((t) => (t as any).expr || (t as any).rawSql || (t as any).query || '')
                    .filter((q: string) => Boolean(q)),
            []
        ),
        timeRange: {
            from: safe(() => String(panelContext.timeRange.from), ''),
            to: safe(() => String(panelContext.timeRange.to), ''),
        },
        datasourceUids: safe(
            () =>
                Array.from(
                    new Set(
                        (panelContext.targets ?? [])
                            .map((t) => (t as any).datasource?.uid)
                            // A datasource set through a dashboard variable
                            // arrives as the raw "${datasource}" string. Passing
                            // that on as if it were a UID feeds a bogus value
                            // into the model's tool calls.
                            .filter((uid: unknown): uid is string => typeof uid === 'string' && uid.length > 0 && !uid.startsWith('$'))
                    )
                ),
            []
        ),
        displayedData: summarizePanelData(panelContext),
        createdAt: Date.now(),
    };
}

/** Stores a handoff and returns the id to put in the new tab's URL, or
 *  undefined when storage is unavailable (private browsing, blocked cookies)
 *  -- the caller then opens a plain chat rather than nothing at all. */
export function storePanelHandoff(handoff: PanelHandoff): string | undefined {
    const id = `${handoff.createdAt.toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
    return safe(() => {
        prune(handoff.createdAt);
        window.localStorage.setItem(KEY_PREFIX + id, JSON.stringify(handoff));
        return id;
    }, undefined);
}

/** Reads a handoff by id. Non-destructive on purpose: reloading the chat tab
 *  must not lose the panel it was opened for. */
export function readPanelHandoff(id: string | null | undefined): PanelHandoff | undefined {
    if (!id) {
        return undefined;
    }
    return safe(() => {
        const raw = window.localStorage.getItem(KEY_PREFIX + id);
        if (!raw) {
            return undefined;
        }
        const parsed = JSON.parse(raw) as PanelHandoff;
        if (Date.now() - parsed.createdAt > MAX_AGE_MS) {
            window.localStorage.removeItem(KEY_PREFIX + id);
            return undefined;
        }
        return parsed;
    }, undefined);
}
