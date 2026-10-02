import { Form } from '@/components/ui/form';
import { TooltipProvider } from '@/components/ui/tooltip';
import { useBuildSwitchOperatorOptions } from '@/hooks/logic-hooks/use-build-operator-options';
import { useFetchKnowledgeMetadata } from '@/hooks/use-knowledge-request';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { useForm } from 'react-hook-form';
import { z } from 'zod';
import { MetadataFilterSchema } from '.';
import { MetadataBoost } from './metadata-boost';

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

const BOOST = {
  method: 'semi_auto',
  semi_auto: [{ key: 'department', op: '=', weight: 0.03 }],
  manual: [
    { key: 'status', op: 'in', value: ['approved', 'final'], weight: 0.05 },
    { key: 'ext', op: 'in', value: ['doc', 'pdf'], weight: 0.05 },
  ],
  max_total: 0.13,
};

let submitted: any;

function Harness({ boost }: { boost: Record<string, any> }) {
  const form = useForm({
    shouldUnregister: false,
    defaultValues: { meta_data_filter: { method: 'semi_auto', boost } },
  });
  return (
    <TooltipProvider>
      <Form {...form}>
        <form
          onSubmit={form.handleSubmit((values) => {
            submitted = values;
          })}
        >
          <MetadataBoost kbIds={['kb']} />
          <button type="submit">save</button>
        </form>
      </Form>
    </TooltipProvider>
  );
}

describe('MetadataBoost', () => {
  beforeEach(() => {
    submitted = undefined;
    (useBuildSwitchOperatorOptions as jest.Mock).mockReturnValue([
      { label: '=', value: '=' },
      { label: 'in', value: 'in' },
    ]);
    (useFetchKnowledgeMetadata as jest.Mock).mockReturnValue({
      data: {
        status: { approved: 1 },
        ext: { pdf: 1 },
        department: { finance: 1 },
      },
    });
  });

  it('shows model-chosen keys and fixed preferences side by side in semi-automatic mode', () => {
    render(<Harness boost={BOOST} />);
    expect(screen.getByText('chat.boostKeys')).toBeInTheDocument();
    expect(screen.getByText('chat.boostFixedConditions')).toBeInTheDocument();
  });

  it('hides both lists when the boost is disabled', () => {
    render(<Harness boost={{ ...BOOST, method: 'disabled' }} />);
    expect(screen.queryByText('chat.boostKeys')).toBeNull();
    expect(screen.queryByText('chat.boostFixedConditions')).toBeNull();
  });

  it('saves the loaded boost unchanged', async () => {
    const { container } = render(<Harness boost={BOOST} />);
    fireEvent.submit(container.querySelector('form')!);
    await waitFor(() => expect(submitted).toBeDefined());
    expect(submitted.meta_data_filter.boost).toEqual(BOOST);
  });

  it('passes the chat form schema without losing a field', () => {
    const filter = {
      method: 'semi_auto',
      semi_auto: [{ key: 'region', op: 'contains' }],
      boost: BOOST,
    };
    expect(
      z.object(MetadataFilterSchema).parse({ meta_data_filter: filter }),
    ).toEqual({
      meta_data_filter: filter,
    });
  });
});
