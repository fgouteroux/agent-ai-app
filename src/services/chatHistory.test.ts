const publishMock = jest.fn();

jest.mock('@grafana/runtime', () => ({
  config: { bootData: { user: { id: 1, orgId: 1 } } },
  getAppEvents: () => ({ publish: publishMock }),
}));

import { config } from '@grafana/runtime';
import { chatHistoryService } from './chatHistory';

const mockConfig = config as unknown as { bootData: { user: { id?: number; orgId?: number } } };

describe('chatHistoryService storage key scoping', () => {
  beforeEach(() => {
    localStorage.clear();
    publishMock.mockClear();
    mockConfig.bootData = { user: { id: 1, orgId: 1 } };
  });

  // Security-audit finding M-06: storage used to be scoped by user id only,
  // so the same person switching between two Grafana orgs they belong to
  // kept seeing the other org's conversation history (dashboard names,
  // incident details) leak into the one they're currently in.
  test('the same user in a different org does not see the other org history', () => {
    chatHistoryService.saveSession([{ role: 'user', content: 'org 1 secret project name' }]);
    expect(chatHistoryService.getAllSessions()).toHaveLength(1);

    mockConfig.bootData = { user: { id: 1, orgId: 2 } };

    expect(chatHistoryService.getAllSessions()).toHaveLength(0);

    chatHistoryService.saveSession([{ role: 'user', content: 'org 2 unrelated question' }]);
    expect(chatHistoryService.getAllSessions()).toHaveLength(1);

    mockConfig.bootData = { user: { id: 1, orgId: 1 } };
    const org1Sessions = chatHistoryService.getAllSessions();
    expect(org1Sessions).toHaveLength(1);
    expect(org1Sessions[0].messages[0].content).toBe('org 1 secret project name');
  });

  test('two different users never share history even in the same org', () => {
    chatHistoryService.saveSession([{ role: 'user', content: 'user A conversation' }]);

    mockConfig.bootData = { user: { id: 2, orgId: 1 } };
    expect(chatHistoryService.getAllSessions()).toHaveLength(0);
  });
});

// Security-audit finding M9: only count/age-based caps existed before --
// nothing bounded the actual serialized byte size, so a handful of large
// conversations could still blow past a real browser's localStorage quota.
describe('chatHistoryService storage byte-size budget', () => {
  const bigContent = (sizeBytes: number) => 'x'.repeat(sizeBytes);

  beforeEach(() => {
    localStorage.clear();
    publishMock.mockClear();
    mockConfig.bootData = { user: { id: 1, orgId: 1 } };
  });

  function totalStoredBytes(): number {
    const raw = localStorage.getItem(`agent_ai_chat_history_1_1`);
    return raw ? new TextEncoder().encode(raw).length : 0;
  }

  test('drops the oldest unpinned sessions once total size exceeds the 4MB budget, keeping the newest', () => {
    // 4 sessions at ~1.5MB of message content each (~6MB total) exceed the
    // 4MB budget -- each gets its own sessionId so they're independent
    // sessions, not the same one being updated in place.
    const ids = ['s1', 's2', 's3', 's4'];
    for (const id of ids) {
      chatHistoryService.saveSession([{ role: 'user', content: bigContent(1_500_000) }], id);
    }

    expect(totalStoredBytes()).toBeLessThanOrEqual(4 * 1024 * 1024);

    const remainingIds = chatHistoryService.getAllSessions().map((s) => s.id);
    // The most recently saved session must never be the one dropped --
    // that would mean the conversation the user just had vanished instantly.
    expect(remainingIds).toContain('s4');
    // And at least the very first one must be gone -- proving pruning
    // actually happened, not just "everything happened to fit".
    expect(remainingIds).not.toContain('s1');

    expect(publishMock).toHaveBeenCalledWith(
      expect.objectContaining({ payload: [expect.stringContaining('removed')] })
    );
  });

  test('never drops a pinned session for size, even if it is the oldest', () => {
    chatHistoryService.saveSession([{ role: 'user', content: bigContent(1_500_000) }], 'pin-me');
    const pinned = chatHistoryService.togglePinSession('pin-me');
    expect(pinned).toBe(true);

    // Push well past the budget with newer, unpinned sessions.
    for (const id of ['s2', 's3', 's4']) {
      chatHistoryService.saveSession([{ role: 'user', content: bigContent(1_500_000) }], id);
    }

    const remaining = chatHistoryService.getAllSessions();
    expect(remaining.find((s) => s.id === 'pin-me')).toBeDefined();
    expect(remaining.find((s) => s.id === 'pin-me')?.isPinned).toBe(true);
  });

  test('a single session larger than the entire budget is dropped rather than silently corrupting storage', () => {
    chatHistoryService.saveSession([{ role: 'user', content: bigContent(5 * 1024 * 1024) }], 'too-big');

    // Nothing else to prune, so the oversized session itself is the one
    // that gets dropped -- storage stays valid and under budget rather
    // than growing unboundedly or throwing.
    expect(chatHistoryService.getAllSessions().find((s) => s.id === 'too-big')).toBeUndefined();
    expect(totalStoredBytes()).toBeLessThanOrEqual(4 * 1024 * 1024);
    expect(publishMock).toHaveBeenCalled();
  });

  test('notifies (does not just console.error silently) when localStorage.setItem itself throws', () => {
    const setItemSpy = jest.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('The quota has been exceeded.', 'QuotaExceededError');
    });
    const consoleSpy = jest.spyOn(console, 'error').mockImplementation(() => {});

    expect(() => chatHistoryService.saveSession([{ role: 'user', content: 'small' }])).not.toThrow();

    expect(publishMock).toHaveBeenCalledWith(
      expect.objectContaining({ payload: [expect.stringContaining("storage is full")] })
    );

    setItemSpy.mockRestore();
    consoleSpy.mockRestore();
  });

  test('sessions well under the budget are never pruned', () => {
    chatHistoryService.saveSession([{ role: 'user', content: 'a perfectly normal short message' }], 'normal');
    expect(chatHistoryService.getAllSessions()).toHaveLength(1);
    expect(publishMock).not.toHaveBeenCalled();
  });
});
