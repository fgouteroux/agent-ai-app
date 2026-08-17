// External libraries
import { useRef, useCallback, RefObject } from 'react';

/**
 * Return type for the auto-scroll hook
 */
interface UseAutoScrollReturn {
    messagesEndRef: RefObject<HTMLDivElement>;
    messageListRef: RefObject<HTMLDivElement>;
    scrollToBottom: (behavior?: ScrollBehavior) => void;
    handleScroll: () => void;
    scrollDownPage: () => void;
    showScrollButton: boolean;
    shouldAutoScroll: boolean;
    setShouldAutoScroll: (value: boolean) => void;
    setShowScrollButton: (value: boolean) => void;
}

/**
 * Props for the useAutoScroll hook
 */
interface UseAutoScrollProps {
    shouldAutoScroll: boolean;
    setShouldAutoScroll: (value: boolean) => void;
    showScrollButton: boolean;
    setShowScrollButton: (value: boolean) => void;
}

/**
 * Custom hook to handle auto-scrolling behavior for message lists
 * Manages scroll-to-bottom functionality and scroll button visibility
 * 
 * @param props Configuration for auto-scroll behavior
 * @returns Refs and handlers for auto-scroll functionality
 */
export const useAutoScroll = (props: UseAutoScrollProps): UseAutoScrollReturn => {
    const { shouldAutoScroll, setShouldAutoScroll, showScrollButton, setShowScrollButton } = props;

    const messageListRef = useRef<HTMLDivElement>(null);
    const messagesEndRef = useRef<HTMLDivElement>(null);
    // Real, reproduced bug: scrollToBottom('smooth') fires a stream of native
    // 'scroll' events over its ~300-500ms animation -- while a response is
    // actively streaming in (messageList's scrollHeight growing every chunk
    // at the same time), handleScroll samples scrollTop/scrollHeight at
    // whatever mid-animation instant it happens to fire and can compute
    // "not at bottom" purely from that race, calling
    // setShouldAutoScroll(false) even though the user never touched the
    // scrollbar -- which then permanently stops the auto-scroll effect from
    // following the rest of that same response. This ref marks a scroll as
    // self-inflicted so handleScroll can ignore it; only a real user-driven
    // scroll (wheel/touch/drag/keyboard, none of which set this flag) may
    // ever disable auto-scroll.
    const programmaticScrollRef = useRef(false);
    const programmaticScrollTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);

    const scrollToBottom = useCallback((behavior: ScrollBehavior = 'smooth') => {
        programmaticScrollRef.current = true;
        if (programmaticScrollTimeoutRef.current) {
            clearTimeout(programmaticScrollTimeoutRef.current);
        }
        // Covers 'smooth' scrolling's own animation window (extended per call
        // so back-to-back streaming chunks keep the flag up continuously,
        // never re-arming handleScroll mid-response) -- 'auto' (instant)
        // resolves in a single frame, well inside this window too.
        programmaticScrollTimeoutRef.current = setTimeout(() => {
            programmaticScrollRef.current = false;
        }, 600);
        // Use requestAnimationFrame for smoother scrolling
        requestAnimationFrame(() => {
            const container = messageListRef.current;
            if (!container) { return; }
            // Real, reproduced bug, confirmed with live scrollHeight/scrollTop
            // measurements: scrollIntoView({block: 'end'}) on messagesEndRef
            // (an empty sentinel div) consistently undershoots the true
            // bottom by exactly messageList's own padding-bottom (the space
            // reserved for the translucent inputArea overlay, sized
            // dynamically via inputAreaBottomPadding) -- it aligns the
            // sentinel with the content box's edge, not the padding box's,
            // leaving that reserved space permanently "unscrolled into" even
            // though it sits between the last real content and the true
            // scrollable end. Setting scrollTop directly against the
            // container's own scrollHeight has no such distinction and
            // reliably reaches the actual end.
            if (behavior === 'smooth' && typeof container.scrollTo === 'function') {
                container.scrollTo({ top: container.scrollHeight, behavior: 'smooth' });
            } else {
                container.scrollTop = container.scrollHeight;
            }
        });
    }, []);

    const handleScroll = useCallback(() => {
        if (programmaticScrollRef.current) {
            return;
        }
        if (messageListRef.current) {
            const { scrollTop, scrollHeight, clientHeight } = messageListRef.current;
            const isAtBottom = Math.abs(scrollHeight - clientHeight - scrollTop) < 50;
            setShowScrollButton(!isAtBottom);
            // Track if user should auto-scroll based on their position
            setShouldAutoScroll(isAtBottom);
        }
    }, [setShowScrollButton, setShouldAutoScroll]);

    const scrollDownPage = useCallback(() => {
        if (messageListRef.current && typeof messageListRef.current.scrollTo === 'function') {
            const { scrollTop, clientHeight } = messageListRef.current;
            messageListRef.current.scrollTo({ top: scrollTop + clientHeight, behavior: 'smooth' });
        }
    }, []);

    return {
        messagesEndRef,
        messageListRef,
        scrollToBottom,
        handleScroll,
        scrollDownPage,
        showScrollButton,
        shouldAutoScroll,
        setShouldAutoScroll,
        setShowScrollButton,
    };
};
