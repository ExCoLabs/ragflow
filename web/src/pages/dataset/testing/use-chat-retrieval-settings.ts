import { useFetchChatById } from '@/hooks/use-chat-request';
import { isEmpty } from 'lodash';
import { useMemo } from 'react';
import { useSearchParams } from 'react-router';
import {
  ChatSearchParam,
  mapChatToRetrievalTestValues,
} from './chat-retrieval-settings';

export function useChatRetrievalSettings() {
  const [searchParams] = useSearchParams();
  const chatId = searchParams.get(ChatSearchParam) ?? undefined;
  const { data: chat } = useFetchChatById(chatId);

  return useMemo(() => {
    if (!chatId || isEmpty(chat)) {
      return undefined;
    }
    return {
      chatName: chat.name,
      values: mapChatToRetrievalTestValues(chat),
    };
  }, [chat, chatId]);
}
