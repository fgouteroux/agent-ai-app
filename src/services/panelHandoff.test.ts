import type { PluginExtensionPanelContext } from '@grafana/data';
import { FieldType } from '@grafana/data';

import { panelHandoffFromContext, readPanelHandoff, storePanelHandoff } from './panelHandoff';

const panelContext = (overrides: Record<string, unknown> = {}): Readonly<PluginExtensionPanelContext> =>
    ({
        id: 7,
        title: 'Error rate',
        timeZone: 'utc',
        timeRange: { from: 'now-6h', to: 'now' },
        dashboard: { uid: 'dash-1', title: 'Checkout' },
        targets: [{ refId: 'A', expr: 'rate(errors[5m])', datasource: { uid: 'prom-uid' } }],
        data: {
            series: [
                {
                    name: 'A',
                    fields: [
                        { name: 'Time', type: FieldType.time, config: {}, values: [1700000000000] },
                        { name: 'value', type: FieldType.number, config: {}, values: [42] },
                    ],
                },
            ],
        },
        ...overrides,
    } as unknown as Readonly<PluginExtensionPanelContext>);

describe('panelHandoffFromContext', () => {
    it('carries what a conversation about this panel needs', () => {
        const handoff = panelHandoffFromContext(panelContext());

        expect(handoff.title).toBe('Error rate');
        expect(handoff.panelId).toBe(7);
        expect(handoff.queries).toEqual(['rate(errors[5m])']);
        expect(handoff.datasourceUids).toEqual(['prom-uid']);
        // The dashboard uid is what lets a follow-up reach the OTHER panels of
        // the same dashboard through get_dashboard.
        expect(handoff.dashboardUid).toBe('dash-1');
        // The values, summarized in the tab that had them -- the new tab never
        // runs the query itself.
        expect(handoff.displayedData).toContain('min=42');
    });

    it('drops a datasource that is really an unresolved dashboard variable', () => {
        const handoff = panelHandoffFromContext(
            panelContext({ targets: [{ refId: 'A', expr: 'up', datasource: { uid: '${datasource}' } }] })
        );

        expect(handoff.datasourceUids).toEqual([]);
    });

    it('still produces a usable handoff from a context that throws on read', () => {
        // Grafana's read-only proxy throws on some properties; a panel we
        // cannot fully describe must still open a chat.
        const hostile = {
            title: 'Error rate',
            timeRange: { from: 'now-1h', to: 'now' },
            get targets(): never {
                throw new TypeError('proxy set handler returned false');
            },
        } as unknown as Readonly<PluginExtensionPanelContext>;

        const handoff = panelHandoffFromContext(hostile);
        expect(handoff.title).toBe('Error rate');
        expect(handoff.queries).toEqual([]);
    });
});

describe('panel handoff storage', () => {
    beforeEach(() => localStorage.clear());

    it('round-trips a handoff through the id put in the new tab URL', () => {
        const id = storePanelHandoff(panelHandoffFromContext(panelContext()));
        expect(id).toBeDefined();

        const restored = readPanelHandoff(id);
        expect(restored?.title).toBe('Error rate');
        expect(restored?.dashboardUid).toBe('dash-1');
    });

    it('survives being read twice -- reloading the chat tab must not lose the panel', () => {
        const id = storePanelHandoff(panelHandoffFromContext(panelContext()));

        expect(readPanelHandoff(id)).toBeDefined();
        expect(readPanelHandoff(id)).toBeDefined();
    });

    it('ignores an expired handoff and forgets it', () => {
        const stale = { ...panelHandoffFromContext(panelContext()), createdAt: Date.now() - 60 * 60 * 1000 };
        const id = storePanelHandoff(stale);

        expect(readPanelHandoff(id)).toBeUndefined();
        expect(localStorage.length).toBe(0);
    });

    it('prunes other stale handoffs when a new one is stored', () => {
        const stale = { ...panelHandoffFromContext(panelContext()), createdAt: Date.now() - 60 * 60 * 1000 };
        storePanelHandoff(stale);
        storePanelHandoff(panelHandoffFromContext(panelContext()));

        // Only the fresh one is left: the store stays bounded without anything
        // running in the background.
        expect(localStorage.length).toBe(1);
    });

    it('returns no id when storage is unavailable, so the caller opens a plain chat', () => {
        const setItem = jest.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
            throw new Error('QuotaExceededError');
        });

        expect(storePanelHandoff(panelHandoffFromContext(panelContext()))).toBeUndefined();
        setItem.mockRestore();
    });

    it('reads nothing for a missing or absent id', () => {
        expect(readPanelHandoff(null)).toBeUndefined();
        expect(readPanelHandoff('never-stored')).toBeUndefined();
    });
});
