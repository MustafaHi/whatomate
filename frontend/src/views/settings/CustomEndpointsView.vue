<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { customEndpointsService, accountsService, chatbotService } from '@/services/api'
import { useAuthStore } from '@/stores/auth'
import { Button } from '@/components/ui/button'
import { Switch } from '@/components/ui/switch'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'
import { PageHeader, DataTable, SearchInput, CrudFormDialog, DeleteConfirmDialog, IconButton, ErrorState, type Column } from '@/components/shared'
import { toast } from 'vue-sonner'
import { Plus, Trash2, Pencil, Braces, Copy } from 'lucide-vue-next'
import { getErrorMessage } from '@/lib/api-utils'
import { formatDate } from '@/lib/utils'
import { useSearchPagination } from '@/composables/useSearchPagination'

const { t } = useI18n()
const authStore = useAuthStore()

interface CustomEndpoint {
  id: string
  name: string
  account_name: string
  flow_id: string
  flow_name?: string
  is_active: boolean
  last_used_at?: string | null
  created_at: string
}

interface AccountOption {
  id: string
  name: string
}

interface FlowOption {
  id: string
  name: string
}

const endpoints = ref<CustomEndpoint[]>([])
const accounts = ref<AccountOption[]>([])
const flows = ref<FlowOption[]>([])
const isLoading = ref(false)
const isDeleting = ref(false)
const isSubmitting = ref(false)
const error = ref<string | null>(null)

const canWrite = computed(() => authStore.hasPermission('flows.chatbot', 'write'))
const canDelete = computed(() => authStore.hasPermission('flows.chatbot', 'delete'))

const isFormOpen = ref(false)
const isEditing = ref(false)
const editingId = ref<string | null>(null)
const formData = ref({ name: '', account_name: '', flow_id: '' })

const isDeleteDialogOpen = ref(false)
const endpointToDelete = ref<CustomEndpoint | null>(null)

// Token reveal: shown once, right after create.
const isTokenOpen = ref(false)
const createdUrl = ref('')
const createdCurl = ref('')

const { searchQuery, currentPage, totalItems, pageSize, handlePageChange } = useSearchPagination({
  fetchFn: () => fetchItems()
})

const columns = computed<Column<CustomEndpoint>[]>(() => [
  { key: 'name', label: t('customEndpoints.name'), sortable: true },
  { key: 'flow', label: t('customEndpoints.flow') },
  { key: 'account', label: t('customEndpoints.account') },
  { key: 'last_used', label: t('customEndpoints.lastUsed'), sortable: true, sortKey: 'last_used_at' },
  { key: 'status', label: t('customEndpoints.status'), sortable: true, sortKey: 'is_active' },
  { key: 'actions', label: t('common.actions'), align: 'right' }
])

const sortKey = ref('name')
const sortDirection = ref<'asc' | 'desc'>('asc')

async function fetchItems() {
  isLoading.value = true
  error.value = null
  try {
    const response = await customEndpointsService.list()
    const data = (response.data as any).data || response.data
    endpoints.value = data.custom_endpoints || []
    totalItems.value = data.total ?? endpoints.value.length
  } catch (err) {
    toast.error(getErrorMessage(err, t('common.failedLoad', { resource: t('resources.customEndpoints') })))
    error.value = t('customEndpoints.errorLoading')
  } finally {
    isLoading.value = false
  }
}

async function fetchOptions() {
  try {
    const [accountsRes, flowsRes] = await Promise.all([accountsService.list(), chatbotService.listFlows({ limit: 100 })])
    accounts.value = ((accountsRes.data as any).data || accountsRes.data)?.accounts || []
    flows.value = ((flowsRes.data as any).data || flowsRes.data)?.flows || []
  } catch (err) {
    toast.error(getErrorMessage(err, t('customEndpoints.failedLoadOptions')))
  }
}

function openCreate() {
  isEditing.value = false
  editingId.value = null
  formData.value = { name: '', account_name: '', flow_id: '' }
  fetchOptions()
  isFormOpen.value = true
}

function openEdit(endpoint: CustomEndpoint) {
  isEditing.value = true
  editingId.value = endpoint.id
  formData.value = { name: endpoint.name, account_name: endpoint.account_name, flow_id: endpoint.flow_id }
  fetchOptions()
  isFormOpen.value = true
}

async function submitForm() {
  if (!formData.value.name || !formData.value.account_name || !formData.value.flow_id) {
    toast.error(t('customEndpoints.requiredFields'))
    return
  }
  isSubmitting.value = true
  try {
    if (isEditing.value && editingId.value) {
      await customEndpointsService.update(editingId.value, { ...formData.value })
      toast.success(t('common.updatedSuccess', { resource: t('resources.customEndpoint') }))
      isFormOpen.value = false
    } else {
      const response = await customEndpointsService.create({ ...formData.value })
      const created = ((response.data as any).data || response.data) as any
      const basePath = ((window as any).__BASE_PATH__ ?? '').replace(/\/$/, '')
      createdUrl.value = `${window.location.origin}${basePath}${created.invoke_url}`
      createdCurl.value = `curl -X POST ${createdUrl.value} \\\n  -H "Content-Type: application/json" \\\n  -d '{"phone": "+15551234567", "variables": {"my_var": "value"}}'`
      isFormOpen.value = false
      isTokenOpen.value = true
    }
    await fetchItems()
  } catch (err) {
    toast.error(getErrorMessage(err, isEditing.value ? t('common.failedUpdate', { resource: t('resources.customEndpoint') }) : t('common.failedCreate', { resource: t('resources.customEndpoint') })))
  } finally {
    isSubmitting.value = false
  }
}

async function deleteEndpoint() {
  if (!endpointToDelete.value) return
  isDeleting.value = true
  try {
    await customEndpointsService.delete(endpointToDelete.value.id)
    await fetchItems()
    toast.success(t('common.deletedSuccess', { resource: t('resources.customEndpoint') }))
    isDeleteDialogOpen.value = false
    endpointToDelete.value = null
  } catch (err) {
    toast.error(getErrorMessage(err, t('common.failedDelete', { resource: t('resources.customEndpoint') })))
  } finally {
    isDeleting.value = false
  }
}

async function toggleActive(endpoint: CustomEndpoint) {
  try {
    await customEndpointsService.update(endpoint.id, { is_active: !endpoint.is_active })
    await fetchItems()
    toast.success(endpoint.is_active ? t('common.disabledSuccess', { resource: t('resources.customEndpoint') }) : t('common.enabledSuccess', { resource: t('resources.customEndpoint') }))
  } catch (e) {
    toast.error(getErrorMessage(e, t('common.failedToggle', { resource: t('resources.customEndpoint') })))
  }
}

async function copyText(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    toast.success(t('common.copied'))
  } catch {
    toast.error(t('customEndpoints.copyFailed'))
  }
}

function formatDateTime(dateStr: string | null | undefined) {
  return dateStr ? formatDate(dateStr, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : t('customEndpoints.never')
}

onMounted(() => fetchItems())
</script>

<template>
  <div class="flex flex-col h-full bg-[#0a0a0b] light:bg-gray-50">
    <PageHeader :title="$t('customEndpoints.title')" :subtitle="$t('customEndpoints.subtitle')" :icon="Braces" icon-gradient="bg-gradient-to-br from-emerald-500 to-teal-600 shadow-emerald-500/20" back-link="/settings">
      <template #actions>
        <Button v-if="canWrite" variant="outline" size="sm" @click="openCreate">
          <Plus class="h-4 w-4 mr-2" />{{ $t('customEndpoints.create') }}
        </Button>
      </template>
    </PageHeader>

    <ScrollArea class="flex-1">
      <div class="p-6">
        <ErrorState
          v-if="error && !isLoading"
          :title="$t('common.loadErrorTitle')"
          :description="error"
          :retry-label="$t('common.retry')"
          @retry="fetchItems"
        />
        <Card v-else>
          <CardHeader>
            <div class="flex items-center justify-between flex-wrap gap-4">
              <div>
                <CardTitle>{{ $t('customEndpoints.yourEndpoints') }}</CardTitle>
                <CardDescription>{{ $t('customEndpoints.yourEndpointsDesc') }}</CardDescription>
              </div>
              <SearchInput v-model="searchQuery" :placeholder="$t('customEndpoints.search') + '...'" class="w-64" />
            </div>
          </CardHeader>
          <CardContent>
            <DataTable
              :items="endpoints"
              :columns="columns"
              :is-loading="isLoading"
              :empty-icon="Braces"
              :empty-title="searchQuery ? $t('customEndpoints.noMatching') : $t('customEndpoints.noneYet')"
              :empty-description="searchQuery ? $t('customEndpoints.noMatchingDesc') : $t('customEndpoints.noneYetDesc')"
              v-model:sort-key="sortKey"
              v-model:sort-direction="sortDirection"
              server-pagination
              :current-page="currentPage"
              :total-items="totalItems"
              :page-size="pageSize"
              item-name="custom endpoints"
              @page-change="handlePageChange"
            >
              <template #cell-name="{ item: endpoint }">
                <span class="font-medium">{{ endpoint.name }}</span>
              </template>
              <template #cell-flow="{ item: endpoint }">{{ endpoint.flow_name || endpoint.flow_id }}</template>
              <template #cell-account="{ item: endpoint }">{{ endpoint.account_name }}</template>
              <template #cell-last_used="{ item: endpoint }">{{ formatDateTime(endpoint.last_used_at) }}</template>
              <template #cell-status="{ item: endpoint }">
                <div class="flex items-center gap-2">
                  <Switch :checked="endpoint.is_active" :disabled="!canWrite" @update:checked="toggleActive(endpoint)" />
                  <span class="text-sm text-muted-foreground">{{ endpoint.is_active ? $t('common.active') : $t('common.inactive') }}</span>
                </div>
              </template>
              <template #cell-actions="{ item: endpoint }">
                <div class="flex items-center justify-end gap-1">
                  <IconButton v-if="canWrite" :icon="Pencil" :label="$t('common.edit')" class="h-8 w-8" @click="openEdit(endpoint)" />
                  <IconButton v-if="canDelete" :icon="Trash2" :label="$t('customEndpoints.deleteLabel')" variant="ghost" class="h-8 w-8 text-destructive" @click="endpointToDelete = endpoint; isDeleteDialogOpen = true" />
                </div>
              </template>
              <template #empty-action>
                <Button v-if="canWrite" variant="outline" size="sm" @click="openCreate">
                  <Plus class="h-4 w-4 mr-2" />{{ $t('customEndpoints.create') }}
                </Button>
              </template>
            </DataTable>
          </CardContent>
        </Card>
      </div>
    </ScrollArea>

    <CrudFormDialog
      v-model:open="isFormOpen"
      :is-editing="isEditing"
      :create-title="$t('customEndpoints.createTitle')"
      :create-description="$t('customEndpoints.createDesc')"
      :edit-title="$t('customEndpoints.editTitle')"
      :edit-description="$t('customEndpoints.editDesc')"
      :is-submitting="isSubmitting"
      max-width="max-w-lg"
      @submit="submitForm"
    >
      <div class="space-y-4">
        <div class="space-y-2">
          <Label for="endpoint-name">{{ $t('customEndpoints.name') }}</Label>
          <Input id="endpoint-name" v-model="formData.name" :placeholder="$t('customEndpoints.namePlaceholder')" />
        </div>
        <div class="space-y-2">
          <Label>{{ $t('customEndpoints.account') }}</Label>
          <Select v-model="formData.account_name">
            <SelectTrigger><SelectValue :placeholder="$t('customEndpoints.selectAccount')" /></SelectTrigger>
            <SelectContent>
              <SelectItem v-for="account in accounts" :key="account.id" :value="account.name">{{ account.name }}</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <div class="space-y-2">
          <Label>{{ $t('customEndpoints.flow') }}</Label>
          <Select v-model="formData.flow_id">
            <SelectTrigger><SelectValue :placeholder="$t('customEndpoints.selectFlow')" /></SelectTrigger>
            <SelectContent>
              <SelectItem v-for="flow in flows" :key="flow.id" :value="flow.id">{{ flow.name }}</SelectItem>
            </SelectContent>
          </Select>
          <p class="text-xs text-muted-foreground">{{ $t('customEndpoints.flowHint') }}</p>
        </div>
      </div>
    </CrudFormDialog>

    <Dialog v-model:open="isTokenOpen">
      <DialogContent class="max-w-lg">
        <DialogHeader>
          <DialogTitle>{{ $t('customEndpoints.createdTitle') }}</DialogTitle>
          <DialogDescription>{{ $t('customEndpoints.createdWarning') }}</DialogDescription>
        </DialogHeader>
        <div class="space-y-4">
          <div class="space-y-2">
            <Label>{{ $t('customEndpoints.invokeUrl') }}</Label>
            <div class="flex items-center gap-2">
              <code class="bg-muted px-2 py-1.5 rounded text-xs break-all flex-1">{{ createdUrl }}</code>
              <IconButton :icon="Copy" :label="$t('common.copy')" class="h-8 w-8 shrink-0" @click="copyText(createdUrl)" />
            </div>
          </div>
          <div class="space-y-2">
            <Label>{{ $t('customEndpoints.curlExample') }}</Label>
            <div class="relative">
              <pre class="bg-muted px-3 py-2 rounded text-xs overflow-x-auto whitespace-pre-wrap">{{ createdCurl }}</pre>
              <IconButton :icon="Copy" :label="$t('common.copy')" class="h-8 w-8 absolute top-1 right-1" @click="copyText(createdCurl)" />
            </div>
          </div>
        </div>
      </DialogContent>
    </Dialog>

    <DeleteConfirmDialog v-model:open="isDeleteDialogOpen" :title="$t('customEndpoints.deleteTitle')" :item-name="endpointToDelete?.name" :description="$t('customEndpoints.deleteWarning')" :is-submitting="isDeleting" @confirm="deleteEndpoint" />
  </div>
</template>
