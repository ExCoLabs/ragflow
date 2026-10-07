import { IDialog, PromptConfig } from '@/interfaces/database/chat';

// Query parameter naming the chat whose saved retrieval settings prefill the
// retrieval testing form.
export const ChatSearchParam = 'chat';

// Maps a chat's saved settings to the retrieval testing request so a test run
// retrieves what the chat would retrieve for the same question.
export function mapChatToRetrievalTestValues(chat: IDialog) {
  const promptConfig: Partial<PromptConfig> = chat.prompt_config ?? {};
  return {
    dataset_ids: chat.dataset_ids,
    similarity_threshold: chat.similarity_threshold,
    keywords_similarity_weight: chat.keywords_similarity_weight,
    page_size: chat.top_n,
    knn_top_k: chat.top_k,
    rerank_candidates_count: chat.rerank_candidates_count,
    rerank_id: chat.rerank_id ?? '',
    cross_languages: promptConfig.cross_languages ?? [],
    meta_data_filter: chat.meta_data_filter,
    keyword: !!promptConfig.keyword,
    use_kg: !!promptConfig.use_kg,
    toc_enhance: !!promptConfig.toc_enhance,
    // The chat's LLM drives metadata filtering, cross-language and keyword
    // extraction on the server, as it does during a chat turn.
    chat_id: chat.llm_id,
    // Chats retrieve knowledge-compilation chunks too.
    include_knowledge_compilation: true,
  };
}

export type ChatRetrievalTestValues = ReturnType<
  typeof mapChatToRetrievalTestValues
>;
