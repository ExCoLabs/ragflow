import '@/locales/config';
import { render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router';
import { TestingResult } from '../testing-result';

type ITestingChunk = import('@/interfaces/database/dataset').ITestingChunk;

// src/routes.tsx builds a browser router at module scope, which jsdom cannot
// do (no global Request). Only the parsed-result path is needed here.
jest.mock('@/routes', () => ({
  Routes: { ParsedResult: '/chunk/parsed' },
}));

// The real module pulls the whole request stack (axios, SSE streams) into
// jsdom; only the route-derived dataset id matters here.
jest.mock('@/hooks/use-knowledge-request', () => ({
  useKnowledgeBaseId: () => 'kb-1',
}));

jest.mock('@/components/list-filter-bar', () => ({
  FilterButton: () => null,
}));

jest.mock('@/components/list-filter-bar/filter-popover', () => ({
  FilterPopover: ({ children }: { children: React.ReactNode }) => (
    <div>{children}</div>
  ),
}));

jest.mock('@/components/empty/empty', () => ({
  __esModule: true,
  default: ({ children }: { children: React.ReactNode }) => (
    <div>{children}</div>
  ),
}));

function chunk(overrides: Partial<ITestingChunk> = {}): ITestingChunk {
  return {
    id: 'chunk-1',
    content: 'Retrieved text',
    content_ltks: '',
    document_id: 'doc-1',
    document_keyword: 'handbook.pdf',
    image_id: '',
    important_keywords: [],
    dataset_id: '',
    similarity: 0.9,
    term_similarity: 0.8,
    vector_similarity: 0.7,
    highlight: '',
    positions: [],
    doc_type_kwd: '',
    ...overrides,
  };
}

type IRetrievalMetaFilter =
  import('@/interfaces/database/dataset').IRetrievalMetaFilter;

function renderResult(
  chunks: ITestingChunk[],
  meta_filter?: IRetrievalMetaFilter,
) {
  return render(
    <MemoryRouter initialEntries={['/dataset/retrieval/kb-1']}>
      <Routes>
        <Route
          path="/dataset/retrieval/:id"
          element={
            <TestingResult
              data={{ chunks, doc_aggs: [], total: chunks.length, meta_filter }}
              loading={false}
              filterValue={{}}
              handleFilterSubmit={jest.fn()}
            />
          }
        />
        <Route path="*" element={null} />
      </Routes>
    </MemoryRouter>,
  );
}

describe('TestingResult', () => {
  it('links the document name to the chunk browser with the chunk id', () => {
    renderResult([chunk()]);

    expect(screen.getByRole('link', { name: 'handbook.pdf' })).toHaveAttribute(
      'href',
      '/chunk/parsed/chunks?id=kb-1&doc_id=doc-1&chunk_id=chunk-1',
    );
  });

  it('prefers the dataset id carried by the chunk itself', () => {
    renderResult([chunk({ dataset_id: 'kb-other' })]);

    expect(screen.getByRole('link', { name: 'handbook.pdf' })).toHaveAttribute(
      'href',
      '/chunk/parsed/chunks?id=kb-other&doc_id=doc-1&chunk_id=chunk-1',
    );
  });

  // Retrieval results only live in memory, so a same-tab navigation would
  // throw them away; the link also has to survive a middle click.
  it('opens the document in a new tab', () => {
    renderResult([chunk()]);
    const link = screen.getByRole('link', { name: 'handbook.pdf' });

    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noreferrer');
  });

  it('leaves the result body inert so its text stays selectable', () => {
    renderResult([chunk()]);

    expect(screen.queryByRole('button')).not.toBeInTheDocument();
    expect(screen.getByText('Retrieved text')).toBeInTheDocument();
  });

  describe('metadata filter summary', () => {
    function metaFilter(
      overrides: Partial<IRetrievalMetaFilter> = {},
    ): IRetrievalMetaFilter {
      return {
        method: 'auto',
        logic: 'and',
        conditions: [
          { key: 'year', op: '=', value: '2026' },
          { key: 'author', op: 'in', value: ['ann', 'bo'] },
        ],
        status: 'applied',
        matched_document_count: 3,
        ...overrides,
      };
    }

    it('shows the conditions the LLM generated and what they matched', () => {
      renderResult([chunk()], metaFilter());

      expect(screen.getByText('Metadata filter (auto)')).toBeInTheDocument();
      expect(screen.getByText('year = 2026')).toBeInTheDocument();
      expect(screen.getByText('author in ann, bo')).toBeInTheDocument();
      expect(screen.getByText('and')).toBeInTheDocument();
      expect(screen.getByText('Narrowed to 3 documents')).toBeInTheDocument();
    });

    it('says so when the filter matched nothing and was dropped', () => {
      renderResult(
        [chunk()],
        metaFilter({ status: 'no_matches', matched_document_count: 0 }),
      );

      expect(
        screen.getByText(
          'Matched no documents, so the whole dataset was searched.',
        ),
      ).toBeInTheDocument();
    });

    it('reports an empty generation rather than showing nothing', () => {
      renderResult(
        [],
        metaFilter({
          status: 'not_generated',
          conditions: [],
          matched_document_count: 0,
        }),
      );

      expect(
        screen.getByText('No conditions were generated.'),
      ).toBeInTheDocument();
      expect(screen.queryByText(/Narrowed to/)).not.toBeInTheDocument();
    });

    it('reports a manual filter that matched nothing as zero documents', () => {
      renderResult(
        [],
        metaFilter({
          method: 'manual',
          status: 'no_matches',
          matched_document_count: 0,
        }),
      );

      expect(screen.getByText('Narrowed to 0 documents')).toBeInTheDocument();
      expect(
        screen.queryByText(
          'Matched no documents, so the whole dataset was searched.',
        ),
      ).not.toBeInTheDocument();
    });

    it('stays out of the way when no metadata filter ran', () => {
      renderResult([chunk()]);

      expect(
        screen.queryByText('Metadata filter (auto)'),
      ).not.toBeInTheDocument();
    });
  });
});
