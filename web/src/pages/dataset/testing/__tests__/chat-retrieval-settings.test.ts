import { IDialog } from '@/interfaces/database/chat';
import { mapChatToRetrievalTestValues } from '../chat-retrieval-settings';

const chat = {
  id: 'chat-1',
  name: 'Support bot',
  llm_id: 'glm-4@ZHIPU',
  dataset_ids: ['kb-1', 'kb-2'],
  similarity_threshold: 0.35,
  keywords_similarity_weight: 0.3,
  top_n: 6,
  top_k: 512,
  rerank_candidates_count: 32,
  rerank_id: 'bge-reranker@BAAI',
  meta_data_filter: { method: 'manual', manual: [] },
  prompt_config: {
    keyword: true,
    use_kg: false,
    toc_enhance: true,
    cross_languages: ['English', 'Slovak'],
  },
} as unknown as IDialog;

describe('mapChatToRetrievalTestValues', () => {
  it('maps the chat retrieval settings onto the testing request', () => {
    expect(mapChatToRetrievalTestValues(chat)).toEqual({
      dataset_ids: ['kb-1', 'kb-2'],
      similarity_threshold: 0.35,
      keywords_similarity_weight: 0.3,
      page_size: 6,
      knn_top_k: 512,
      rerank_candidates_count: 32,
      rerank_id: 'bge-reranker@BAAI',
      cross_languages: ['English', 'Slovak'],
      meta_data_filter: { method: 'manual', manual: [] },
      keyword: true,
      use_kg: false,
      toc_enhance: true,
      chat_id: 'glm-4@ZHIPU',
      include_knowledge_compilation: true,
    });
  });

  it('falls back to empty values for unset optional settings', () => {
    const values = mapChatToRetrievalTestValues({
      ...chat,
      rerank_id: undefined,
      prompt_config: undefined,
    } as unknown as IDialog);
    expect(values.rerank_id).toBe('');
    expect(values.cross_languages).toEqual([]);
    expect(values.keyword).toBe(false);
    expect(values.use_kg).toBe(false);
    expect(values.toc_enhance).toBe(false);
  });
});
