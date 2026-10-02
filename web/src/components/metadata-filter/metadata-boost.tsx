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

import { Button } from '@/components/ui/button';
import {
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form';
import { Input } from '@/components/ui/input';
import { ComparisonOperator } from '@/constants/agent';
import { DatasetMetadata } from '@/constants/chat';
import { useTranslate } from '@/hooks/common-hooks';
import { useBuildSwitchOperatorOptions } from '@/hooks/logic-hooks/use-build-operator-options';
import { useFetchKnowledgeMetadata } from '@/hooks/use-knowledge-request';
import { Plus, X } from 'lucide-react';
import { useCallback, useMemo } from 'react';
import { useFieldArray, useFormContext, useWatch } from 'react-hook-form';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { SelectWithSearch } from '../originui/select-with-search';
import { RAGFlowFormItem } from '../ragflow-form';
import { InputSelect } from '../ui/input-select';

// Mirrors internal/common/chunk_metadata.go: MetaBoostOps, DefaultMetaBoostWeight, DefaultMetaBoostMaxTotal.
export const BoostExtremumOperator = {
  Max: 'max',
  Min: 'min',
} as const;

const BoostComparisonOperators = [
  ComparisonOperator.Equal,
  ComparisonOperator.In,
  ComparisonOperator.GreatThan,
  ComparisonOperator.GreatEqual,
  ComparisonOperator.LessThan,
  ComparisonOperator.LessEqual,
  ComparisonOperator.Contains,
];

const DEFAULT_AUTO_WEIGHT = 0.15;
const DEFAULT_MAX_TOTAL = 0.3;

const weightSchema = z.coerce.number().min(0).max(1);

export const MetadataBoostSchema = z
  .object({
    method: z.string().optional(),
    manual: z
      .array(
        z.object({
          key: z.string(),
          op: z.string(),
          value: z.union([z.string(), z.array(z.string())]).optional(),
          weight: weightSchema.optional(),
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
            weight: weightSchema.optional(),
          }),
        ]),
      )
      .optional(),
    auto_weight: weightSchema.optional(),
    max_total: weightSchema.optional(),
  })
  .optional();

type MetadataBoostProps = {
  kbIds: string[];
  prefix?: string;
};

function isExtremum(op: string | undefined) {
  return op === BoostExtremumOperator.Max || op === BoostExtremumOperator.Min;
}

function useBoostOperatorOptions() {
  const { t } = useTranslation();
  const comparison = useBuildSwitchOperatorOptions(BoostComparisonOperators);
  return useMemo(
    () => [
      ...comparison,
      { value: BoostExtremumOperator.Max, label: t('chat.boostOpMax') },
      { value: BoostExtremumOperator.Min, label: t('chat.boostOpMin') },
    ],
    [comparison, t],
  );
}

function WeightField({
  name,
  className,
}: {
  name: string;
  className?: string;
}) {
  const { t } = useTranslation();
  const form = useFormContext();
  return (
    <FormField
      control={form.control}
      name={name}
      render={({ field }) => (
        <FormItem className={className}>
          <FormControl>
            <Input
              type="number"
              min={0}
              max={1}
              step={0.05}
              placeholder={t('chat.boostWeight')}
              className="bg-bg-input"
              value={field.value ?? ''}
              onChange={(e) =>
                field.onChange(
                  e.target.value === '' ? undefined : Number(e.target.value),
                )
              }
            />
          </FormControl>
          <FormMessage />
        </FormItem>
      )}
    />
  );
}

/** Fixed preferences — key, operator, value (none for max/min), weight. Applied in every enabled mode. */
function BoostManualRows({
  name,
  metadata,
  label,
}: {
  name: string;
  metadata: ReturnType<typeof useFetchKnowledgeMetadata>;
  label: string;
}) {
  const { t } = useTranslation();
  const form = useFormContext();
  const operatorOptions = useBoostOperatorOptions();
  const { fields, remove, append } = useFieldArray({
    name,
    control: form.control,
  });
  const rows = useWatch({ control: form.control, name });

  const metadataOptions = useMemo(
    () =>
      Object.keys(metadata.data || {}).map((key) => ({
        label: key,
        value: key,
      })),
    [metadata.data],
  );

  const valueOptions = useCallback(
    (key: string | undefined) => {
      const values = key ? metadata.data?.[key] : undefined;
      if (!values || typeof values !== 'object') return [];
      return Object.keys(values).map((item) => ({ value: item, label: item }));
    },
    [metadata.data],
  );

  const add = useCallback(() => {
    append({
      key: '',
      op: ComparisonOperator.Equal,
      value: '',
      weight: DEFAULT_AUTO_WEIGHT,
    });
  }, [append]);

  return (
    <section className="flex flex-col gap-2">
      <div className="flex items-center justify-between">
        <FormLabel>{label}</FormLabel>
        <Button
          variant={'outline'}
          type="button"
          size="sm"
          onClick={add}
          className="h-8"
        >
          <Plus className="mr-2 size-4" />
          {t('common.add')}
        </Button>
      </div>
      <div className="space-y-2">
        {fields.map((field, index) => {
          const op: string | undefined = rows?.[index]?.op;
          const key: string | undefined = rows?.[index]?.key;
          return (
            <section key={field.id} className="flex items-start gap-2">
              <FormField
                control={form.control}
                name={`${name}.${index}.key`}
                render={({ field }) => (
                  <FormItem className="flex-[2] overflow-hidden">
                    <FormControl>
                      <SelectWithSearch
                        {...field}
                        options={metadataOptions}
                        placeholder={t('common.pleaseSelect')}
                        triggerClassName="bg-bg-input"
                        value={field.value}
                        onChange={field.onChange}
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name={`${name}.${index}.op`}
                render={({ field }) => (
                  <FormItem className="flex-[2]">
                    <FormControl>
                      <SelectWithSearch
                        {...field}
                        options={operatorOptions}
                        triggerClassName="bg-bg-input"
                        value={field.value}
                        onChange={(value) => {
                          field.onChange(value);
                          if (isExtremum(value)) {
                            form.setValue(`${name}.${index}.value`, '');
                          } else if (
                            value === ComparisonOperator.In ||
                            op === ComparisonOperator.In
                          ) {
                            form.setValue(
                              `${name}.${index}.value`,
                              value === ComparisonOperator.In ? [] : '',
                            );
                          }
                        }}
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              {!isExtremum(op) && (
                <FormField
                  control={form.control}
                  name={`${name}.${index}.value`}
                  render={({ field }) => (
                    <FormItem className="flex-[2] min-w-0">
                      <FormControl>
                        <InputSelect
                          placeholder={t('common.pleaseInput')}
                          {...field}
                          options={valueOptions(key)}
                          className="w-full"
                          multi={op === ComparisonOperator.In}
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              )}
              <WeightField name={`${name}.${index}.weight`} className="w-24" />
              <Button
                variant={'ghost'}
                size="icon"
                type="button"
                onClick={() => remove(index)}
                className="mt-0 h-8 w-10"
              >
                <X className="size-4 text-text-sub-title-invert" />
              </Button>
            </section>
          );
        })}
      </div>
    </section>
  );
}

/** Semi-automatic mode: keys the model may prefer on; operator and weight can be pinned. */
function BoostSemiAutoRows({
  name,
  metadata,
}: {
  name: string;
  metadata: ReturnType<typeof useFetchKnowledgeMetadata>;
}) {
  const { t } = useTranslation();
  const form = useFormContext();
  const operatorOptions = useBoostOperatorOptions();
  const { fields, remove, append } = useFieldArray({
    name,
    control: form.control,
  });
  const autoOption = { label: t('chat.meta.auto'), value: '' };
  const metadataOptions = useMemo(
    () =>
      Object.keys(metadata.data || {}).map((key) => ({
        label: key,
        value: key,
      })),
    [metadata.data],
  );
  const add = useCallback(() => {
    append({ key: '', op: '', weight: undefined });
  }, [append]);

  return (
    <section className="flex flex-col gap-2">
      <div className="flex items-center justify-between">
        <FormLabel>{t('chat.boostKeys')}</FormLabel>
        <Button
          variant={'outline'}
          type="button"
          size="sm"
          onClick={add}
          className="h-8"
        >
          <Plus className="mr-2 size-4" />
          {t('common.add')}
        </Button>
      </div>
      <div className="space-y-2">
        {fields.map((field, index) => (
          <section key={field.id} className="flex items-start gap-2">
            <FormField
              control={form.control}
              name={`${name}.${index}.key`}
              render={({ field }) => (
                <FormItem className="flex-[2] overflow-hidden">
                  <FormControl>
                    <SelectWithSearch
                      {...field}
                      options={metadataOptions}
                      placeholder={t('common.pleaseSelect')}
                      triggerClassName="bg-bg-input"
                      value={field.value}
                      onChange={field.onChange}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name={`${name}.${index}.op`}
              render={({ field }) => (
                <FormItem className="flex-[2]">
                  <FormControl>
                    <SelectWithSearch
                      {...field}
                      options={[autoOption, ...operatorOptions]}
                      triggerClassName="bg-bg-input"
                      value={field.value ?? ''}
                      onChange={field.onChange}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <WeightField name={`${name}.${index}.weight`} className="w-24" />
            <Button
              variant={'ghost'}
              size="icon"
              type="button"
              onClick={() => remove(index)}
              className="mt-0 h-8 w-10"
            >
              <X className="size-4 text-text-sub-title-invert" />
            </Button>
          </section>
        ))}
      </div>
    </section>
  );
}

/**
 * Metadata boost: preferences that raise a chunk's score without excluding the
 * others. Same three modes as the filter above it, configured independently;
 * the backend (common.chunk_metadata) needs the dataset to have chunk metadata
 * enabled and backfilled, otherwise the boost is ignored.
 */
export function MetadataBoost({ kbIds, prefix = '' }: MetadataBoostProps) {
  const { t } = useTranslate('chat');
  const form = useFormContext();
  const base = prefix + 'meta_data_filter.boost';
  const methodName = `${base}.method`;
  const method = useWatch({ control: form.control, name: methodName });
  const metadata = useFetchKnowledgeMetadata(kbIds);

  const methodOptions = Object.values(DatasetMetadata).map((x) => ({
    value: x,
    label: t(`meta.${x}`),
  }));
  const enabled = method && method !== DatasetMetadata.Disabled;

  return (
    <>
      <RAGFlowFormItem
        label={t('metadataBoost')}
        name={methodName}
        tooltip={t('metadataBoostTip')}
      >
        <SelectWithSearch
          options={methodOptions}
          triggerClassName="!bg-bg-input"
        />
      </RAGFlowFormItem>
      {method === DatasetMetadata.SemiAutomatic && (
        <BoostSemiAutoRows name={`${base}.semi_auto`} metadata={metadata} />
      )}
      {enabled && (
        <BoostManualRows
          name={`${base}.manual`}
          metadata={metadata}
          label={
            method === DatasetMetadata.Manual
              ? t('boostConditions')
              : t('boostFixedConditions')
          }
        />
      )}
      {(method === DatasetMetadata.Automatic ||
        method === DatasetMetadata.SemiAutomatic) && (
        <RAGFlowFormItem
          label={t('boostAutoWeight')}
          name={`${base}.auto_weight`}
          tooltip={t('boostAutoWeightTip')}
        >
          <Input
            type="number"
            min={0}
            max={1}
            step={0.05}
            placeholder={String(DEFAULT_AUTO_WEIGHT)}
            className="bg-bg-input"
          />
        </RAGFlowFormItem>
      )}
      {enabled && (
        <RAGFlowFormItem
          label={t('boostMaxTotal')}
          name={`${base}.max_total`}
          tooltip={t('boostMaxTotalTip')}
        >
          <Input
            type="number"
            min={0}
            max={1}
            step={0.05}
            placeholder={String(DEFAULT_MAX_TOTAL)}
            className="bg-bg-input"
          />
        </RAGFlowFormItem>
      )}
    </>
  );
}
