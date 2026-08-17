// Chat session and history related type definitions

import { Message } from './llm.types';

/**
 * A saved chat session with metadata
 */
export interface ChatSession {
    id: string;
    title: string;
    messages: Message[];
    createdAt: number;
    updatedAt: number;
    isPinned?: boolean;
    /** Agent ID active when this session was last saved (e.g. "agent-1"). Absent/'generic' = Default. */
    agent?: string;
    /** Display label of `agent`, snapshotted at save time so history renders even if the agent catalog changes later. */
    agentLabel?: string;
}
