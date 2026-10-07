import { Form } from '@/components/ui/form';
import { TooltipProvider } from '@/components/ui/tooltip';
import { useBuildSwitchOperatorOptions } from '@/hooks/logic-hooks/use-build-operator-options';
import { useFetchKnowledgeMetadata } from '@/hooks/use-knowledge-request';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { useForm } from 'react-hook-form';
import { MetadataFilter } from '.';

jest.mock('@/hooks/logic-hooks/use-build-operator-options', () => ({
  useBuildSwitchOperatorOptions: jest.fn(),
}));

jest.mock('@/hooks/use-knowledge-request', () => ({
  useFetchKnowledgeMetadata: jest.fn(),
}));

jest.mock('@/pages/agent/form/components/prompt-editor', () => ({
  PromptEditor: () => null,
}));

// index.tsx also exports the agent variant, whose hooks pull in the app's i18n setup.
jest.mock('./agent-metadata-filter-conditions', () => ({
  AgentMetadataFilterConditions: () => null,
}));

jest.mock('react-i18next', () => ({
  useTranslation: (_ns?: string, options?: { keyPrefix?: string }) => ({
    t: (key: string) =>
      options?.keyPrefix ? `${options.keyPrefix}.${key}` : key,
  }),
}));

let submitted: any;

function Harness({ filter }: { filter: Record<string, any> }) {
  const form = useForm({
    defaultValues: { kb_ids: ['kb'], meta_data_filter: filter },
  });
  return (
    <TooltipProvider>
      <Form {...form}>
        <form
          onSubmit={form.handleSubmit((values) => {
            submitted = values;
          })}
        >
          <MetadataFilter />
          <button type="submit">save</button>
        </form>
      </Form>
    </TooltipProvider>
  );
}

const instructions = () =>
  screen.queryByPlaceholderText('chat.metadataInstructionsPlaceholder');

describe('MetadataFilter instructions', () => {
  beforeEach(() => {
    submitted = undefined;
    (useBuildSwitchOperatorOptions as jest.Mock).mockReturnValue([]);
    (useFetchKnowledgeMetadata as jest.Mock).mockReturnValue({ data: {} });
  });

  it.each([
    [{ method: 'disabled' }, false],
    [{ method: 'manual', manual: [] }, false],
    [{ method: 'auto' }, true],
    [{ method: 'semi_auto', semi_auto: [] }, true],
    [{ method: 'disabled', boost: { method: 'auto' } }, true],
  ])('%j shows the field: %s', (filter, shown) => {
    render(<Harness filter={filter} />);
    expect(!!instructions()).toBe(shown);
  });

  it('saves the text with the filter config', async () => {
    render(<Harness filter={{ method: 'auto' }} />);
    fireEvent.change(instructions()!, {
      target: { value: 'FY means fiscal year' },
    });
    fireEvent.click(screen.getByText('save'));
    await waitFor(() =>
      expect(submitted.meta_data_filter.instructions).toBe(
        'FY means fiscal year',
      ),
    );
  });
});
