/*
 *  Copyright 2026 The InfiniFlow Authors. All Rights Reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 */

import { DatasetMetadata } from '@/constants/chat';
import { useTranslate } from '@/hooks/common-hooks';
import { useFormContext, useWatch } from 'react-hook-form';
import { z } from 'zod';
import { SelectWithSearch } from '../originui/select-with-search';
import { RAGFlowFormItem } from '../ragflow-form';
import { Textarea } from '../ui/textarea';
import { AgentMetadataFilterConditions } from './agent-metadata-filter-conditions';
import { MetadataBoost, MetadataBoostSchema } from './metadata-boost';
import { MetadataFilterConditions } from './metadata-filter-conditions';
import { MetadataSemiAutoFields } from './metadata-semi-auto-fields';

// Mirrors MetaFilterInstructionsLimit in internal/service/metadata_filter.go.
const META_FILTER_INSTRUCTIONS_LIMIT = 4000;

const LLM_METHODS: string[] = [
  DatasetMetadata.Automatic,
  DatasetMetadata.SemiAutomatic,
];

type MetadataFilterProps = {
  prefix?: string;
  canReference?: boolean;
};

export const MetadataFilterSchema = {
  meta_data_filter: z
    .object({
      logic: z.string().optional(),
      method: z.string().optional(),
      manual: z
        .array(
          z.object({
            key: z.string(),
            op: z.string(),
            value: z.union([z.string(), z.array(z.string())]),
          }),
        )
        .optional(),
      semi_auto: z
        .array(
          z.union([
            z.string(),
            z.object({
              key: z.string(),
              op: z.string().optional(),
            }),
          ]),
        )
        .optional(),
      boost: MetadataBoostSchema,
      instructions: z.string().max(META_FILTER_INSTRUCTIONS_LIMIT).optional(),
    })
    .optional(),
};

export function MetadataFilter({
  prefix = '',
  canReference,
}: MetadataFilterProps) {
  const { t } = useTranslate('chat');
  const form = useFormContext();

  const methodName = prefix + 'meta_data_filter.method';

  const datasetIds: string[] = useWatch({
    control: form.control,
    name: prefix + 'dataset_ids',
  });

  const oldKbIds: string[] = useWatch({
    control: form.control,
    name: prefix + 'kb_ids',
  });

  const kbIds = datasetIds || oldKbIds || [];

  const metadata = useWatch({
    control: form.control,
    name: methodName,
  });
  const boostMethod = useWatch({
    control: form.control,
    name: prefix + 'meta_data_filter.boost.method',
  });
  const hasKnowledge = Array.isArray(kbIds) && kbIds.length > 0;
  const usesLlm =
    LLM_METHODS.includes(metadata) || LLM_METHODS.includes(boostMethod);

  const MetadataOptions = Object.values(DatasetMetadata).map((x) => {
    return {
      value: x,
      label: t(`meta.${x}`),
    };
  });

  const Conditions = canReference
    ? AgentMetadataFilterConditions
    : MetadataFilterConditions;

  return (
    <>
      {hasKnowledge && (
        <RAGFlowFormItem
          label={t('metadata')}
          name={methodName}
          tooltip={t('metadataTip')}
        >
          <SelectWithSearch
            options={MetadataOptions}
            triggerClassName="!bg-bg-input"
          />
        </RAGFlowFormItem>
      )}
      {hasKnowledge && metadata === DatasetMetadata.Manual && (
        <Conditions kbIds={kbIds} prefix={prefix}></Conditions>
      )}
      {hasKnowledge && metadata === DatasetMetadata.SemiAutomatic && (
        <MetadataSemiAutoFields
          kbIds={kbIds}
          prefix={prefix}
        ></MetadataSemiAutoFields>
      )}
      {hasKnowledge && <MetadataBoost kbIds={kbIds} prefix={prefix} />}
      {hasKnowledge && usesLlm && (
        <RAGFlowFormItem
          label={t('metadataInstructions')}
          name={prefix + 'meta_data_filter.instructions'}
          tooltip={t('metadataInstructionsTip')}
        >
          <Textarea
            rows={4}
            maxLength={META_FILTER_INSTRUCTIONS_LIMIT}
            placeholder={t('metadataInstructionsPlaceholder')}
            className="bg-bg-input"
          />
        </RAGFlowFormItem>
      )}
    </>
  );
}
