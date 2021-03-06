// Copyright (C) 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vulkan

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/golang/protobuf/proto"
	"github.com/google/gapid/core/image"
	"github.com/google/gapid/core/log"
	"github.com/google/gapid/core/stream"
	"github.com/google/gapid/gapis/api"
	"github.com/google/gapid/gapis/memory"
	"github.com/google/gapid/gapis/shadertools"
)

type imagePrimer struct {
	sb *stateBuilder
	rh *ipRenderHandler
	sh *ipImageStoreHandler
}

func newImagePrimer(sb *stateBuilder) *imagePrimer {
	p := &imagePrimer{
		sb: sb,
		rh: newImagePrimerRenderHandler(sb),
		sh: newImagePrimerStoreHandler(sb),
	}
	return p
}

const (
	stagingColorImageBufferFormat        = VkFormat_VK_FORMAT_R32G32B32A32_UINT
	stagingDepthStencilImageBufferFormat = VkFormat_VK_FORMAT_R32_UINT
)

func (p *imagePrimer) free() {
	p.rh.free()
	p.sh.free()
}

// internal functions of image primer

// createImageAndBindMemory creates an image with the give image info and device
// handle in the new state of the state builder of the current image primer,
// allocates memory for the created image based on the given memory type index,
// binds the memory with the new image, returns the created image object and the
// new device memory object in the new state of the state builder of the current
// image primer, and an error if any error occur.
func (p *imagePrimer) createImageAndBindMemory(dev VkDevice, info ImageInfo, memTypeIndex int) (ImageObjectʳ, DeviceMemoryObjectʳ, error) {
	imgHandle := VkImage(newUnusedID(true, func(x uint64) bool {
		return GetState(p.sb.newState).Images().Contains(VkImage(x))
	}))
	vkCreateImage(p.sb, dev, info, imgHandle)
	img := GetState(p.sb.newState).Images().Get(imgHandle)
	// Query the memory requirements so validation layers are happy
	vkGetImageMemoryRequirements(p.sb, dev, imgHandle, MakeVkMemoryRequirements(p.sb.ta))

	imgSize, err := subInferImageSize(p.sb.ctx, nil, api.CmdNoID, nil, p.sb.newState, GetState(p.sb.newState), 0, nil, nil, img)
	if err != nil {
		return ImageObjectʳ{}, DeviceMemoryObjectʳ{}, log.Errf(p.sb.ctx, err, "[Getting image size]")
	}
	memHandle := VkDeviceMemory(newUnusedID(true, func(x uint64) bool {
		return GetState(p.sb.newState).DeviceMemories().Contains(VkDeviceMemory(x))
	}))
	// Since we cannot guess how much the driver will actually request of us,
	// overallocating by a factor of 2 should be enough.
	// TODO: Insert opcodes to determine the allocation size dynamically on the
	// replay side.
	allocSize := VkDeviceSize(imgSize * 2)
	if allocSize < VkDeviceSize(256*1024) {
		allocSize = VkDeviceSize(256 * 1024)
	}
	vkAllocateMemory(p.sb, dev, allocSize, uint32(memTypeIndex), memHandle)
	mem := GetState(p.sb.newState).DeviceMemories().Get(memHandle)

	vkBindImageMemory(p.sb, dev, imgHandle, memHandle, 0)
	return img, mem, nil
}

// createSameStagingImage creates an image with the same image info (except
// initial layout) as the given image along with the given initial layout, and
// create backing memory for the new image and bind the image with the created
// memory (sparse binding not supported). Returns the created image object in
// the new state of the stateBuilder in the image primer, a function to destroy
// the new created image and backing memory, and an error.
func (p *imagePrimer) createSameStagingImage(img ImageObjectʳ, initialLayout VkImageLayout) (ImageObjectʳ, func(), error) {
	dev := p.sb.s.Devices().Get(img.Device())
	phyDevMemProps := p.sb.s.PhysicalDevices().Get(dev.PhysicalDevice()).MemoryProperties()
	// TODO: Handle multi-planar images
	memInfo, _ := subGetImagePlaneMemoryInfo(p.sb.ctx, nil, api.CmdNoID, nil, p.sb.oldState, GetState(p.sb.oldState), 0, nil, nil, img, VkImageAspectFlagBits(0))
	memTypeBits := memInfo.MemoryRequirements().MemoryTypeBits()
	memIndex := memoryTypeIndexFor(memTypeBits, phyDevMemProps, VkMemoryPropertyFlags(VkMemoryPropertyFlagBits_VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT))
	if memIndex < 0 {
		// fallback to use whatever type of memory available
		memIndex = memoryTypeIndexFor(memTypeBits, phyDevMemProps, VkMemoryPropertyFlags(0))
	}
	if memIndex < 0 {
		return ImageObjectʳ{}, func() {}, log.Errf(p.sb.ctx, fmt.Errorf("can't find an appropriate memory type index"), "[Creatig staging image same as image: %v]", img.VulkanHandle())
	}

	createInfo := img.Info()
	createInfo.SetInitialLayout(initialLayout)

	stagingImg, stagingImgMem, err := p.createImageAndBindMemory(img.Device(), createInfo, memIndex)
	if err != nil {
		return ImageObjectʳ{}, func() {}, log.Errf(p.sb.ctx, err, "[Creating staging image same as image: %v]", img.VulkanHandle())
	}
	return stagingImg, func() {
		p.sb.write(p.sb.cb.VkDestroyImage(stagingImg.Device(), stagingImg.VulkanHandle(), memory.Nullptr))
		p.sb.write(p.sb.cb.VkFreeMemory(stagingImgMem.Device(), stagingImgMem.VulkanHandle(), memory.Nullptr))
	}, nil
}

// create32BitUintColorStagingImagesForAspect creates stagining images with format
// RGBA32_UINT for the given image's specific, allocated backing memory for the
// new created images and bind memory for them, returns the created image
// objects in the new state of the state builder of the current image primer, a
// function to destroy the created image and backing memories, and an error in
// case of any error occur.
func (p *imagePrimer) create32BitUintColorStagingImagesForAspect(img ImageObjectʳ, aspect VkImageAspectFlagBits, usages VkImageUsageFlags) ([]ImageObjectʳ, func(), error) {
	stagingImgs := []ImageObjectʳ{}
	stagingMems := []DeviceMemoryObjectʳ{}

	srcElementAndTexelInfo, err := subGetElementAndTexelBlockSize(p.sb.ctx, nil, api.CmdNoID, nil, p.sb.oldState, GetState(p.sb.oldState), 0, nil, nil, img.Info().Fmt())
	if err != nil {
		return []ImageObjectʳ{}, func() {}, log.Errf(p.sb.ctx, err, "[Getting element size and texel block info]")
	}
	if srcElementAndTexelInfo.TexelBlockSize().Width() != 1 || srcElementAndTexelInfo.TexelBlockSize().Height() != 1 {
		// compressed formats are not supported
		return []ImageObjectʳ{}, func() {}, log.Errf(p.sb.ctx, err, "allocating staging images for compressed format images is not supported")
	}
	srcElementSize := srcElementAndTexelInfo.ElementSize()
	if aspect == VkImageAspectFlagBits_VK_IMAGE_ASPECT_DEPTH_BIT {
		srcElementSize, err = subGetDepthElementSize(p.sb.ctx, nil, api.CmdNoID, nil, p.sb.oldState, GetState(p.sb.oldState), 0, nil, nil, img.Info().Fmt(), false)
		if err != nil {
			return []ImageObjectʳ{}, func() {}, log.Errf(p.sb.ctx, err, "[Getting element size for depth aspect] %v", err)
		}
	} else if aspect == VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT {
		srcElementSize = 1
	}

	stagingImgFormat := VkFormat_VK_FORMAT_UNDEFINED
	switch aspect {
	case VkImageAspectFlagBits_VK_IMAGE_ASPECT_COLOR_BIT:
		stagingImgFormat = stagingColorImageBufferFormat
	case VkImageAspectFlagBits_VK_IMAGE_ASPECT_DEPTH_BIT,
		VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT:
		stagingImgFormat = stagingDepthStencilImageBufferFormat
	}
	if stagingImgFormat == VkFormat_VK_FORMAT_UNDEFINED {
		return []ImageObjectʳ{}, func() {}, log.Errf(p.sb.ctx, nil, "unsupported aspect: %v", aspect)
	}
	stagingElementInfo, _ := subGetElementAndTexelBlockSize(p.sb.ctx, nil, api.CmdNoID, nil, p.sb.oldState, GetState(p.sb.oldState), 0, nil, nil, stagingImgFormat)
	stagingElementSize := stagingElementInfo.ElementSize()

	stagingInfo := img.Info().Clone(p.sb.newState.Arena, api.CloneContext{})
	stagingInfo.SetDedicatedAllocationNV(NilDedicatedAllocationBufferImageCreateInfoNVʳ)
	stagingInfo.SetFmt(stagingImgFormat)
	stagingInfo.SetUsage(usages)

	dev := p.sb.s.Devices().Get(img.Device())
	phyDevMemProps := p.sb.s.PhysicalDevices().Get(dev.PhysicalDevice()).MemoryProperties()
	// TODO: Handle multi-planar images
	memInfo, _ := subGetImagePlaneMemoryInfo(p.sb.ctx, nil, api.CmdNoID, nil, p.sb.oldState, GetState(p.sb.oldState), 0, nil, nil, img, VkImageAspectFlagBits(0))
	memTypeBits := memInfo.MemoryRequirements().MemoryTypeBits()
	memIndex := memoryTypeIndexFor(memTypeBits, phyDevMemProps, VkMemoryPropertyFlags(VkMemoryPropertyFlagBits_VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT))
	if memIndex < 0 {
		// fallback to use whatever type of memory available
		memIndex = memoryTypeIndexFor(memTypeBits, phyDevMemProps, VkMemoryPropertyFlags(0))
	}
	if memIndex < 0 {
		return []ImageObjectʳ{}, func() {}, log.Errf(p.sb.ctx, nil, "can't find an appropriate memory type index")
	}

	covered := uint32(0)
	for covered < srcElementSize {
		stagingImg, mem, err := p.createImageAndBindMemory(dev.VulkanHandle(), stagingInfo, memIndex)
		if err != nil {
			return []ImageObjectʳ{}, func() {}, log.Errf(p.sb.ctx, err, "[Creating 32 bit wide staging images for image: %v, aspect: %v, usages: %v]", img.VulkanHandle(), aspect, usages)
		}
		stagingImgs = append(stagingImgs, stagingImg)
		stagingMems = append(stagingMems, mem)
		covered += stagingElementSize
	}

	free := func() {
		for _, img := range stagingImgs {
			p.sb.write(p.sb.cb.VkDestroyImage(img.Device(), img.VulkanHandle(), memory.Nullptr))
		}
		for _, mem := range stagingMems {
			p.sb.write(p.sb.cb.VkFreeMemory(mem.Device(), mem.VulkanHandle(), memory.Nullptr))
		}
	}
	return stagingImgs, free, nil
}

func (p *imagePrimer) createImageViewForImageSubresource(
	img ImageObjectʳ, aspect VkImageAspectFlagBits, layer, level uint32, imgViewType VkImageViewType) (ImageViewObjectʳ, func(), error) {

	if img.IsNil() {
		return ImageViewObjectʳ{}, func() {}, log.Errf(p.sb.ctx, fmt.Errorf("Nil Image object"), "[Creating image view]")
	}
	dev := img.Device()
	imgView := VkImageView(newUnusedID(true, func(x uint64) bool {
		return GetState(p.sb.newState).ImageViews().Contains(VkImageView(x))
	}))
	p.sb.write(p.sb.cb.VkCreateImageView(
		img.Device(),
		NewVkImageViewCreateInfoᶜᵖ(p.sb.MustAllocReadData(
			NewVkImageViewCreateInfo(p.sb.ta,
				VkStructureType_VK_STRUCTURE_TYPE_IMAGE_VIEW_CREATE_INFO, // sType
				0,                  // pNext
				0,                  // flags
				img.VulkanHandle(), // image
				imgViewType,        // viewType
				img.Info().Fmt(),   // format
				NewVkComponentMapping(p.sb.ta, // components
					VkComponentSwizzle_VK_COMPONENT_SWIZZLE_IDENTITY, // r
					VkComponentSwizzle_VK_COMPONENT_SWIZZLE_IDENTITY, // g
					VkComponentSwizzle_VK_COMPONENT_SWIZZLE_IDENTITY, // b
					VkComponentSwizzle_VK_COMPONENT_SWIZZLE_IDENTITY, // a
				),
				NewVkImageSubresourceRange(p.sb.ta, // subresourceRange
					VkImageAspectFlags(aspect), // aspectMask
					level,                      // baseMipLevel
					1,                          // levelCount
					layer,                      // baseArrayLayer
					1,                          // layerCount
				),
			)).Ptr()),
		memory.Nullptr,
		p.sb.MustAllocWriteData(imgView).Ptr(),
		VkResult_VK_SUCCESS,
	))
	free := func() {
		p.sb.write(p.sb.cb.VkDestroyImageView(dev, imgView, memory.Nullptr))
	}
	return GetState(p.sb.newState).ImageViews().Get(imgView), free, nil
}

type ipLayoutInfo interface {
	layoutOf(aspect VkImageAspectFlagBits, layer, level uint32) VkImageLayout
}

type ipLayoutInfoFromImage struct {
	img ImageObjectʳ
}

func (i *ipLayoutInfoFromImage) layoutOf(aspect VkImageAspectFlagBits, layer, level uint32) VkImageLayout {
	if _, ok := i.img.Aspects().Lookup(aspect); !ok {
		return VkImageLayout_VK_IMAGE_LAYOUT_UNDEFINED
	}
	if _, ok := i.img.Aspects().Get(aspect).Layers().Lookup(layer); !ok {
		return VkImageLayout_VK_IMAGE_LAYOUT_UNDEFINED
	}
	if _, ok := i.img.Aspects().Get(aspect).Layers().Get(layer).Levels().Lookup(level); !ok {
		return VkImageLayout_VK_IMAGE_LAYOUT_UNDEFINED
	}
	return i.img.Aspects().Get(aspect).Layers().Get(layer).Levels().Get(level).Layout()
}

func sameLayoutsOfImage(img ImageObjectʳ) ipLayoutInfo {
	return &ipLayoutInfoFromImage{img: img}
}

type ipLayoutInfoFromLayout struct {
	layout VkImageLayout
}

func (i *ipLayoutInfoFromLayout) layoutOf(aspect VkImageAspectFlagBits, layer, level uint32) VkImageLayout {
	return i.layout
}

func useSpecifiedLayout(layout VkImageLayout) ipLayoutInfo {
	return &ipLayoutInfoFromLayout{layout: layout}
}

// In-shader image store handler
type ipImageStoreHandler struct {
	sb              *stateBuilder
	descSetLayouts  map[VkDevice]VkDescriptorSetLayout
	descPools       map[VkDevice]VkDescriptorPool
	descSets        map[VkDevice]VkDescriptorSet
	pipelineLayouts map[VkDevice]VkPipelineLayout
	pipelines       map[ipImageStoreShaderInfo]ComputePipelineObjectʳ
	shaders         map[ipImageStoreShaderInfo]ShaderModuleObjectʳ
}

type ipImageStoreJob struct {
	input      ImageViewObjectʳ
	inputIndex int
	output     ImageViewObjectʳ
	offset     VkOffset3D
	extent     VkExtent3D
}

type ipImageStoreShaderInfo struct {
	dev          VkDevice
	inputFormat  VkFormat
	inputAspect  VkImageAspectFlagBits
	outputFormat VkFormat
	outputAspect VkImageAspectFlagBits
	imgType      VkImageType
}

const (
	ipImageStoreOutputImageBinding   = 0
	ipImageStoreInputImageBinding    = 1
	ipImageStoreUniformBufferBinding = 2
	specMaxComputeGroupCountX        = 65536
	specMaxComputeGroupCountY        = 65536
	specMaxComputeGroupCountZ        = 65536
)

// Interfaces of image store handler to interact with image primer

func newImagePrimerStoreHandler(sb *stateBuilder) *ipImageStoreHandler {
	return &ipImageStoreHandler{
		sb:              sb,
		descSetLayouts:  map[VkDevice]VkDescriptorSetLayout{},
		descPools:       map[VkDevice]VkDescriptorPool{},
		descSets:        map[VkDevice]VkDescriptorSet{},
		pipelineLayouts: map[VkDevice]VkPipelineLayout{},
		pipelines:       map[ipImageStoreShaderInfo]ComputePipelineObjectʳ{},
		shaders:         map[ipImageStoreShaderInfo]ShaderModuleObjectʳ{},
	}
}

func (h *ipImageStoreHandler) store(job ipImageStoreJob, queue VkQueue) error {
	var err error

	dev := job.output.Device()

	if _, ok := h.descPools[dev]; !ok {
		descPool := VkDescriptorPool(newUnusedID(true, func(x uint64) bool {
			return GetState(h.sb.newState).DescriptorPools().Contains(VkDescriptorPool(x))
		}))
		descPoolSizes := []VkDescriptorPoolSize{
			// for output image and input image
			NewVkDescriptorPoolSize(h.sb.ta,
				VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_IMAGE, // Type
				2, // descriptorCount
			),
			// for image dimension info
			NewVkDescriptorPoolSize(h.sb.ta,
				VkDescriptorType_VK_DESCRIPTOR_TYPE_UNIFORM_BUFFER, // Type
				1, // descriptorCount
			),
		}
		vkCreateDescriptorPool(h.sb, dev, VkDescriptorPoolCreateFlags(
			VkDescriptorPoolCreateFlagBits_VK_DESCRIPTOR_POOL_CREATE_FREE_DESCRIPTOR_SET_BIT),
			1, descPoolSizes, descPool)
		h.descPools[dev] = descPool
	}
	descPool := h.descPools[dev]

	// create descriptor set layout
	if _, ok := h.descSetLayouts[dev]; !ok {
		descSetLayoutHandle := VkDescriptorSetLayout(newUnusedID(true, func(x uint64) bool {
			return GetState(h.sb.newState).DescriptorSetLayouts().Contains(VkDescriptorSetLayout(x))
		}))
		bindings := []VkDescriptorSetLayoutBinding{
			NewVkDescriptorSetLayoutBinding(h.sb.ta,
				ipImageStoreOutputImageBinding,                    // binding
				VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_IMAGE, // descriptorType
				1, // descriptorCount
				VkShaderStageFlags(VkShaderStageFlagBits_VK_SHADER_STAGE_COMPUTE_BIT), // stageFlags
				0, // pImmutableSamplers
			),
			NewVkDescriptorSetLayoutBinding(h.sb.ta,
				ipImageStoreInputImageBinding,                            // binding
				VkDescriptorType_VK_DESCRIPTOR_TYPE_UNIFORM_TEXEL_BUFFER, // descriptorType
				1, // descriptorCount
				VkShaderStageFlags(VkShaderStageFlagBits_VK_SHADER_STAGE_COMPUTE_BIT), // stageFlags
				0, // pImmutableSamplers
			),
			NewVkDescriptorSetLayoutBinding(h.sb.ta,
				ipImageStoreUniformBufferBinding,                   // binding
				VkDescriptorType_VK_DESCRIPTOR_TYPE_UNIFORM_BUFFER, // descriptorType
				1, // descriptorCount
				VkShaderStageFlags(VkShaderStageFlagBits_VK_SHADER_STAGE_COMPUTE_BIT), // stageFlags
				0, // pImmutableSamplers
			),
		}
		vkCreateDescriptorSetLayout(h.sb, dev, bindings, descSetLayoutHandle)
		h.descSetLayouts[dev] = descSetLayoutHandle
	}

	// allocate descriptor set
	if _, ok := h.descSets[dev]; !ok {
		descSet := VkDescriptorSet(newUnusedID(true, func(x uint64) bool {
			return GetState(h.sb.newState).DescriptorSets().Contains(VkDescriptorSet(x))
		}))
		vkAllocateDescriptorSet(h.sb, dev, descPool, h.descSetLayouts[dev], descSet)
		h.descSets[dev] = descSet
	}
	descSet := h.descSets[dev]

	// Create compute pipeline
	metaData := make([]uint32, 0, 6)
	metaData = append(metaData,
		uint32(job.offset.X()),
		uint32(job.offset.Y()),
		uint32(job.offset.Z()),
		uint32(job.inputIndex),
	)
	var db bytes.Buffer
	binary.Write(&db, binary.LittleEndian, metaData)
	if _, ok := h.pipelineLayouts[dev]; !ok {
		pipelineLayoutHandle := VkPipelineLayout(newUnusedID(true, func(x uint64) bool {
			return GetState(h.sb.newState).PipelineLayouts().Contains(VkPipelineLayout(x))
		}))
		vkCreatePipelineLayout(h.sb, dev, []VkDescriptorSetLayout{h.descSetLayouts[dev]},
			[]VkPushConstantRange{
				NewVkPushConstantRange(h.sb.ta,
					VkShaderStageFlags(VkShaderStageFlagBits_VK_SHADER_STAGE_FRAGMENT_BIT), // stageFlags
					0,                       // offset
					uint32(len(db.Bytes())), // size
				)}, pipelineLayoutHandle)
		h.pipelineLayouts[dev] = pipelineLayoutHandle
	}
	pipelineLayoutHandle := h.pipelineLayouts[dev]

	if job.input.Image().Info().ImageType() != job.output.Image().Info().ImageType() {
		return log.Errf(h.sb.ctx, fmt.Errorf("input image type: %v != output image type: %v",
			job.input.Image().Info().ImageType(), job.output.Image().Info().ImageType()),
			"[Checking compute pipeline shader info]")
	}
	compShaderInfo := ipImageStoreShaderInfo{
		dev:          dev,
		inputFormat:  job.input.Fmt(),
		inputAspect:  VkImageAspectFlagBits(job.input.SubresourceRange().AspectMask()),
		outputFormat: job.output.Fmt(),
		outputAspect: VkImageAspectFlagBits(job.output.SubresourceRange().AspectMask()),
		imgType:      job.input.Image().Info().ImageType(),
	}
	pipeline, err := h.getOrCreateComputePipeline(compShaderInfo)
	if err != nil {
		return log.Errf(h.sb.ctx, err, "[Getting compute pipeline]")
	}

	// Check store extent dimension.
	// All the compute shader has local size:  local_size_x/y/z = 1, and we make
	// each invocation to process one pixel. This means the dispatch group count
	// in each dimension should equal to the store extent.
	if specMaxComputeGroupCountX < job.extent.Width() {
		return log.Errf(h.sb.ctx, fmt.Errorf("Extent.Width: %v too large", job.extent.Width()), "[Checking imageStore extent dimension]")
	}
	if specMaxComputeGroupCountY < job.extent.Height() {
		return log.Errf(h.sb.ctx, fmt.Errorf("Extent.Height: %v too large", job.extent.Height()), "[Checking imageStore extent dimension]")
	}
	if specMaxComputeGroupCountZ < job.extent.Depth() {
		return log.Errf(h.sb.ctx, fmt.Errorf("Extent.z: %v too large", job.extent.Depth()), "[Checking imageStore extent dimension]")
	}

	tsk := h.sb.newScratchTaskOnQueue(queue)

	// update descriptor sets
	tsk.doOnCommitted(func() {
		writeDescriptorSet(h.sb, dev, descSet, ipImageStoreOutputImageBinding, 0,
			VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_IMAGE, []VkDescriptorImageInfo{
				NewVkDescriptorImageInfo(h.sb.ta,
					0,                                     // Sampler
					job.output.VulkanHandle(),             // ImageView
					VkImageLayout_VK_IMAGE_LAYOUT_GENERAL, // ImageLayout
				),
			}, []VkDescriptorBufferInfo{}, []VkBufferView{},
		)
		writeDescriptorSet(h.sb, dev, descSet, ipImageStoreInputImageBinding, 0,
			VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_IMAGE, []VkDescriptorImageInfo{
				NewVkDescriptorImageInfo(h.sb.ta,
					0,                                     // Sampler
					job.input.VulkanHandle(),              // ImageView
					VkImageLayout_VK_IMAGE_LAYOUT_GENERAL, // ImageLayout
				),
			}, []VkDescriptorBufferInfo{}, []VkBufferView{},
		)
	})

	// command buffer commands
	tsk.recordCmdBufCommand(func(commandBuffer VkCommandBuffer) {
		h.sb.write(h.sb.cb.VkCmdBindPipeline(
			commandBuffer,
			VkPipelineBindPoint_VK_PIPELINE_BIND_POINT_COMPUTE,
			pipeline.VulkanHandle(),
		))
		h.sb.write(h.sb.cb.VkCmdBindDescriptorSets(
			commandBuffer,
			VkPipelineBindPoint_VK_PIPELINE_BIND_POINT_COMPUTE,
			pipelineLayoutHandle,
			0, 1, h.sb.MustAllocReadData(descSet).Ptr(),
			0, NewU32ᶜᵖ(memory.Nullptr),
		))
		h.sb.write(h.sb.cb.VkCmdPushConstants(
			commandBuffer,
			pipelineLayoutHandle,
			VkShaderStageFlags(VkShaderStageFlagBits_VK_SHADER_STAGE_COMPUTE_BIT),
			0,
			uint32(len(db.Bytes())),
			NewCharᶜᵖ(h.sb.MustAllocReadData(db.Bytes()).Ptr()),
		))
		groupCountX := job.extent.Width()
		groupCountY := job.extent.Height()
		groupCountZ := job.extent.Depth()
		h.sb.write(h.sb.cb.VkCmdDispatch(commandBuffer, groupCountX, groupCountY, groupCountZ))
	})

	// commit the task
	if err := tsk.commit(); err != nil {
		log.E(h.sb.ctx, "[Committing scratch task for priming storage image: %v by imageStore, image view subresource: %v ] %v", job.output.Image().VulkanHandle(), job.output.SubresourceRange(), err)
	}
	h.sb.flushQueueFamilyScratchResources(tsk.queue)
	return nil
}

func (h *ipImageStoreHandler) free() {
	for dev, p := range h.pipelines {
		h.sb.write(h.sb.cb.VkDestroyPipeline(p.Device(), p.VulkanHandle(), memory.Nullptr))
		delete(h.pipelines, dev)
	}
	for dev, m := range h.shaders {
		h.sb.write(h.sb.cb.VkDestroyShaderModule(m.Device(), m.VulkanHandle(), memory.Nullptr))
		delete(h.shaders, dev)
	}
	for dev, l := range h.pipelineLayouts {
		h.sb.write(h.sb.cb.VkDestroyPipelineLayout(dev, l, memory.Nullptr))
		delete(h.pipelineLayouts, dev)
	}
	for dev, p := range h.descPools {
		h.sb.write(h.sb.cb.VkDestroyDescriptorPool(dev, p, memory.Nullptr))
		delete(h.descPools, dev)
	}
	for dev, l := range h.descSetLayouts {
		h.sb.write(h.sb.cb.VkDestroyDescriptorSetLayout(dev, l, memory.Nullptr))
		delete(h.descSetLayouts, dev)
	}
}

// Internal functions of image store handler

func (h *ipImageStoreHandler) getOrCreateComputePipeline(info ipImageStoreShaderInfo) (ComputePipelineObjectʳ, error) {

	if p, ok := h.pipelines[info]; ok {
		return p, nil
	}

	compShader, err := h.getOrCreateShaderModule(info)
	// TODO: report to report view if the image is a depth/stencil image.
	if err != nil {
		return NilComputePipelineObjectʳ, log.Errf(h.sb.ctx, err, "[Getting compute shader module]")
	}

	if _, ok := h.pipelineLayouts[info.dev]; !ok {
		return NilComputePipelineObjectʳ, log.Errf(h.sb.ctx, nil, "pipeline layout not found")
	}

	handle := VkPipeline(newUnusedID(true, func(x uint64) bool {
		return GetState(h.sb.newState).ComputePipelines().Contains(VkPipeline(x))
	}))

	createInfo := NewVkComputePipelineCreateInfo(h.sb.ta,
		VkStructureType_VK_STRUCTURE_TYPE_COMPUTE_PIPELINE_CREATE_INFO, // sType
		0, // pNext
		0, // flags
		NewVkPipelineShaderStageCreateInfo(h.sb.ta, // stage
			VkStructureType_VK_STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO, // sType
			0, // pNext
			0, // flags
			VkShaderStageFlagBits_VK_SHADER_STAGE_COMPUTE_BIT, // stage
			compShader.VulkanHandle(),                         // module
			NewCharᶜᵖ(h.sb.MustAllocReadData("main").Ptr()),   // pName
			NewVkSpecializationInfoᶜᵖ(memory.Nullptr),         // pSpecializationInfo
		),
		h.pipelineLayouts[info.dev], // layout
		0,                           // basePipelineHandle
		0,                           // basePipelineIndex
	)
	h.sb.write(h.sb.cb.VkCreateComputePipelines(
		info.dev, VkPipelineCache(0), uint32(1),
		h.sb.MustAllocReadData(createInfo).Ptr(),
		memory.Nullptr, h.sb.MustAllocWriteData(handle).Ptr(),
		VkResult_VK_SUCCESS,
	))
	h.pipelines[info] = GetState(h.sb.newState).ComputePipelines().Get(handle)
	return h.pipelines[info], nil
}

func (h *ipImageStoreHandler) getOrCreateShaderModule(info ipImageStoreShaderInfo) (ShaderModuleObjectʳ, error) {
	if m, ok := h.shaders[info]; ok {
		return m, nil
	}
	handle := VkShaderModule(newUnusedID(true, func(x uint64) bool {
		return GetState(h.sb.newState).ShaderModules().Contains(VkShaderModule(x))
	}))
	code, err := ipComputeShaderSpirv(info.outputFormat, info.outputAspect, info.inputFormat, info.inputAspect, info.imgType)
	if err != nil {
		return NilShaderModuleObjectʳ, log.Errf(h.sb.ctx, err, "[Generating SPIR-V for: %v]", info)
	}
	if len(code) == 0 {
		return NilShaderModuleObjectʳ, log.Errf(h.sb.ctx, nil, "no SPIR-V code generated")
	}
	vkCreateShaderModule(h.sb, info.dev, code, handle)
	h.shaders[info] = GetState(h.sb.newState).ShaderModules().Get(handle)
	return h.shaders[info], nil
}

// Input attachment -> image render handler

type ipRenderJob struct {
	inputAttachmentImages []ipRenderImage
	renderTarget          ipRenderImage
	inputFormat           VkFormat
}

type ipRenderImage struct {
	image         ImageObjectʳ
	aspect        VkImageAspectFlagBits
	layer         uint32
	level         uint32
	initialLayout VkImageLayout
	finalLayout   VkImageLayout
}

const (
	ipRenderInputAttachmentBinding = 0
)

type ipRenderDescriptorSetInfo struct {
	dev                 VkDevice
	numInputAttachments int
	pushConstant        bool
}

type ipRenderPassInfo struct {
	dev                         VkDevice
	numInputAttachments         int
	inputAttachmentImageFormat  VkFormat
	inputAttachmentImageSamples VkSampleCountFlagBits
	targetAspect                VkImageAspectFlagBits
	targetFormat                VkFormat
	targetSamples               VkSampleCountFlagBits
}

type ipRenderShaderInfo struct {
	dev      VkDevice
	isVertex bool
	format   VkFormat
	aspect   VkImageAspectFlagBits
}

type ipGfxPipelineInfo struct {
	fragShaderInfo ipRenderShaderInfo
	pipelineLayout VkPipelineLayout
	renderPassInfo ipRenderPassInfo
}

type ipRenderHandler struct {
	sb *stateBuilder
	// descriptor set layouts indexed by different number of input attachment
	descriptorSetLayouts map[ipRenderDescriptorSetInfo]DescriptorSetLayoutObjectʳ
	// pipeline layouts indexed by the number of input attachment in the only
	// descriptor set layout of the pipeline layout.
	pipelineLayouts map[ipRenderDescriptorSetInfo]PipelineLayoutObjectʳ
	// pipelines indexed by the pipeline info.
	pipelines map[ipGfxPipelineInfo]GraphicsPipelineObjectʳ
	// shader modules indexed by the shader info.
	shaders map[ipRenderShaderInfo]ShaderModuleObjectʳ
	// the fill info for the scratch buffers for vertex buffer and index buffer,
	// the raw content of the those two buffers are supposed to be contants.
	vertexBufferFillInfo *bufferSubRangeFillInfo
	indexBufferFillInfo  *bufferSubRangeFillInfo
}

// Interfaces of render handler to interact with image primer

func newImagePrimerRenderHandler(sb *stateBuilder) *ipRenderHandler {
	return &ipRenderHandler{
		sb:                   sb,
		descriptorSetLayouts: map[ipRenderDescriptorSetInfo]DescriptorSetLayoutObjectʳ{},
		pipelineLayouts:      map[ipRenderDescriptorSetInfo]PipelineLayoutObjectʳ{},
		pipelines:            map[ipGfxPipelineInfo]GraphicsPipelineObjectʳ{},
		shaders:              map[ipRenderShaderInfo]ShaderModuleObjectʳ{},
	}
}

func (h *ipRenderHandler) free() {
	for _, obj := range h.pipelines {
		h.sb.write(h.sb.cb.VkDestroyPipeline(obj.Device(), obj.VulkanHandle(), memory.Nullptr))
	}
	for _, obj := range h.shaders {
		h.sb.write(h.sb.cb.VkDestroyShaderModule(obj.Device(), obj.VulkanHandle(), memory.Nullptr))
	}
	for _, obj := range h.pipelineLayouts {
		h.sb.write(h.sb.cb.VkDestroyPipelineLayout(obj.Device(), obj.VulkanHandle(), memory.Nullptr))
	}
	for _, obj := range h.descriptorSetLayouts {
		h.sb.write(h.sb.cb.VkDestroyDescriptorSetLayout(obj.Device(), obj.VulkanHandle(), memory.Nullptr))
	}
}

func ipImageBarrierAspectFlags(aspect VkImageAspectFlagBits, fmt VkFormat) VkImageAspectFlags {
	switch fmt {
	case VkFormat_VK_FORMAT_D16_UNORM_S8_UINT,
		VkFormat_VK_FORMAT_D24_UNORM_S8_UINT,
		VkFormat_VK_FORMAT_D32_SFLOAT_S8_UINT:
		aspect |= VkImageAspectFlagBits_VK_IMAGE_ASPECT_DEPTH_BIT |
			VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT
	}
	return VkImageAspectFlags(aspect)
}

func (h *ipRenderHandler) render(job *ipRenderJob, tsk *scratchTask) error {
	switch job.renderTarget.aspect {
	case VkImageAspectFlagBits_VK_IMAGE_ASPECT_COLOR_BIT,
		VkImageAspectFlagBits_VK_IMAGE_ASPECT_DEPTH_BIT,
		VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT:
	default:
		return log.Errf(h.sb.ctx, nil, "unsupported aspect: %v", job.renderTarget.aspect)
	}
	outputBarrierAspect := ipImageBarrierAspectFlags(job.renderTarget.aspect, job.renderTarget.image.Info().Fmt())

	var outputPreRenderLayout VkImageLayout
	switch job.renderTarget.aspect {
	case VkImageAspectFlagBits_VK_IMAGE_ASPECT_COLOR_BIT:
		outputPreRenderLayout = VkImageLayout_VK_IMAGE_LAYOUT_COLOR_ATTACHMENT_OPTIMAL
	case VkImageAspectFlagBits_VK_IMAGE_ASPECT_DEPTH_BIT,
		VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT:
		outputPreRenderLayout = VkImageLayout_VK_IMAGE_LAYOUT_DEPTH_STENCIL_ATTACHMENT_OPTIMAL
	default:
		return log.Errf(h.sb.ctx, nil, "unsupported aspect: %v", job.renderTarget.aspect)
	}

	dev := job.renderTarget.image.Device()

	descSetInfo := ipRenderDescriptorSetInfo{
		dev:                 dev,
		numInputAttachments: len(job.inputAttachmentImages),
	}
	if job.renderTarget.aspect == VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT {
		// If the render target aspect is stencil, an uniform buffer is required
		// to store the stencil bit index value.
		descSetInfo.pushConstant = true
	}
	descPool := h.createDescriptorPool(descSetInfo)
	if !descPool.IsNil() {
		tsk.deferUntilExecuted(func() {
			h.sb.write(h.sb.cb.VkDestroyDescriptorPool(dev, descPool.VulkanHandle(), memory.Nullptr))
		})
	} else {
		return log.Errf(h.sb.ctx, nil, "failed to create descriptor pool for %v input attachments", len(job.inputAttachmentImages))
	}
	descSetLayout := h.getOrCreateDescriptorSetLayout(descSetInfo)
	descSet := h.allocDescriptorSet(dev, descPool.VulkanHandle(), descSetLayout.VulkanHandle())
	if !descSet.IsNil() {
		tsk.deferUntilExecuted(func() {
			h.sb.write(h.sb.cb.VkFreeDescriptorSets(
				dev, descSet.DescriptorPool(), 1, NewVkDescriptorSetᶜᵖ(
					h.sb.MustAllocReadData(descSet.VulkanHandle()).Ptr()), VkResult_VK_SUCCESS))
		})
	} else {
		return log.Errf(h.sb.ctx, nil, "failed to allocate descriptorset with %v input attachments", len(job.inputAttachmentImages))
	}

	inputViews := []ImageViewObjectʳ{}
	for _, input := range job.inputAttachmentImages {
		// TODO: support rendering to 3D images if maintenance1 is enabled.
		if input.image.Info().ImageType() == VkImageType_VK_IMAGE_TYPE_3D {
			return log.Errf(h.sb.ctx, nil, "rendering to 3D images are not supported yet")
		}
		view := h.createImageView(dev, input.image, input.aspect, input.layer, input.level)
		inputViews = append(inputViews, view)
		if !view.IsNil() {
			tsk.deferUntilExecuted(func() {
				h.sb.write(h.sb.cb.VkDestroyImageView(dev, view.VulkanHandle(), memory.Nullptr))
			})
		} else {
			return log.Errf(h.sb.ctx, nil, "failed to create image view for input attachment image: %v", input.image.VulkanHandle())
		}
	}
	// TODO: support rendering to 3D images if maintenance1 is enabled.
	if job.renderTarget.image.Info().ImageType() == VkImageType_VK_IMAGE_TYPE_3D {
		return log.Errf(h.sb.ctx, nil, "rendering to 3D images are not supported yet")
	}
	outputView := h.createImageView(dev, job.renderTarget.image, job.renderTarget.aspect, job.renderTarget.layer, job.renderTarget.level)
	if !outputView.IsNil() {
		tsk.deferUntilExecuted(func() {
			h.sb.write(h.sb.cb.VkDestroyImageView(dev, outputView.VulkanHandle(), memory.Nullptr))
		})
	} else {
		return log.Errf(h.sb.ctx, nil, "failed to create image view for rendering target image: %v",
			job.renderTarget.image.VulkanHandle())
	}

	imgInfoList := []VkDescriptorImageInfo{}
	for _, view := range inputViews {
		imgInfoList = append(imgInfoList, NewVkDescriptorImageInfo(h.sb.ta,
			0,                   // Sampler
			view.VulkanHandle(), // ImageView
			VkImageLayout_VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL, // ImageLayout
		))
	}

	tsk.doOnCommitted(func() {
		writeDescriptorSet(h.sb, dev, descSet.VulkanHandle(), ipRenderInputAttachmentBinding, 0, VkDescriptorType_VK_DESCRIPTOR_TYPE_INPUT_ATTACHMENT, imgInfoList, []VkDescriptorBufferInfo{}, []VkBufferView{})
	})

	renderPassInfo := ipRenderPassInfo{
		dev:                         dev,
		numInputAttachments:         len(job.inputAttachmentImages),
		inputAttachmentImageFormat:  job.inputAttachmentImages[0].image.Info().Fmt(),
		inputAttachmentImageSamples: job.inputAttachmentImages[0].image.Info().Samples(),
		targetAspect:                job.renderTarget.aspect,
		targetFormat:                job.renderTarget.image.Info().Fmt(),
		targetSamples:               job.renderTarget.image.Info().Samples(),
	}
	renderPass := h.createRenderPass(renderPassInfo, job.renderTarget.finalLayout)
	if !renderPass.IsNil() {
		tsk.deferUntilExecuted(func() {
			h.sb.write(h.sb.cb.VkDestroyRenderPass(dev, renderPass.VulkanHandle(), memory.Nullptr))
		})
	} else {
		return log.Errf(h.sb.ctx, nil, "failed to create renderpass for rendering")
	}

	allViews := []VkImageView{}
	for _, view := range inputViews {
		allViews = append(allViews, view.VulkanHandle())
	}
	allViews = append(allViews, outputView.VulkanHandle())

	targetLevelSize := h.sb.levelSize(job.renderTarget.image.Info().Extent(),
		job.renderTarget.image.Info().Fmt(), job.renderTarget.level, job.renderTarget.aspect)

	framebuffer := h.createFramebuffer(dev, renderPass.VulkanHandle(), allViews,
		uint32(targetLevelSize.width), uint32(targetLevelSize.height))
	if !framebuffer.IsNil() {
		tsk.deferUntilExecuted(func() {
			h.sb.write(h.sb.cb.VkDestroyFramebuffer(dev, framebuffer.VulkanHandle(), memory.Nullptr))
		})
	} else {
		return log.Errf(h.sb.ctx, nil, "failed to create framebuffer for rendering")
	}

	pipelineLayout := h.getOrCreatePipelineLayout(descSetInfo)
	if pipelineLayout.IsNil() {
		return log.Errf(h.sb.ctx, nil, "failed to get pipeline layout for the rendering")
	}

	pipelineInfo := ipGfxPipelineInfo{
		fragShaderInfo: ipRenderShaderInfo{
			dev:      dev,
			isVertex: false,
			format:   job.inputFormat,
			aspect:   job.renderTarget.aspect,
		},
		pipelineLayout: pipelineLayout.VulkanHandle(),
		renderPassInfo: renderPassInfo,
	}
	pipeline, err := h.getOrCreateGraphicsPipeline(pipelineInfo, renderPass.VulkanHandle())
	if err != nil {
		return log.Errf(h.sb.ctx, err, "[Getting graphics pipeline]")
	}

	inputSrcBarriers := []VkImageMemoryBarrier{}
	dstBarriers := []VkImageMemoryBarrier{}
	for _, input := range job.inputAttachmentImages {
		aspects := ipImageBarrierAspectFlags(input.aspect, input.image.Info().Fmt())
		inputSrcBarriers = append(inputSrcBarriers,
			NewVkImageMemoryBarrier(h.sb.ta,
				VkStructureType_VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER, // sType
				0, // pNext
				VkAccessFlags((VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT-1)|VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT), // srcAccessMask
				VkAccessFlags(VkAccessFlagBits_VK_ACCESS_INPUT_ATTACHMENT_READ_BIT),                                        // dstAccessMask
				input.initialLayout, // oldLayout
				VkImageLayout_VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL, // newLayout
				queueFamilyIgnore,          // srcQueueFamilyIndex
				queueFamilyIgnore,          // dstQueueFamilyIndex
				input.image.VulkanHandle(), // image
				NewVkImageSubresourceRange(h.sb.ta, // subresourceRange
					aspects,                          // aspectMask
					0,                                // baseMipLevel
					input.image.Info().MipLevels(),   // levelCount
					0,                                // baseArrayLayer
					input.image.Info().ArrayLayers(), // layerCount
				),
			))
		if input.finalLayout != VkImageLayout_VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL {
			dstBarriers = append(dstBarriers,
				NewVkImageMemoryBarrier(h.sb.ta,
					VkStructureType_VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER, // sType
					0, // pNext
					VkAccessFlags((VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT-1)|VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT), // srcAccessMask
					VkAccessFlags(VkAccessFlagBits_VK_ACCESS_INPUT_ATTACHMENT_READ_BIT),                                        // dstAccessMask
					VkImageLayout_VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL,                                                     // oldLayout
					input.finalLayout,          // newLayout
					queueFamilyIgnore,          // srcQueueFamilyIndex
					queueFamilyIgnore,          // dstQueueFamilyIndex
					input.image.VulkanHandle(), // image
					NewVkImageSubresourceRange(h.sb.ta, // subresourceRange
						aspects,                          // aspectMask
						0,                                // baseMipLevel
						input.image.Info().MipLevels(),   // levelCount
						0,                                // baseArrayLayer
						input.image.Info().ArrayLayers(), // layerCount
					),
				))
		}
	}
	outputBarrier := NewVkImageMemoryBarrier(h.sb.ta,
		VkStructureType_VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER, // sType
		0, // pNext
		0, // srcAccessMask
		VkAccessFlags(VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT), // dstAccessMask
		GetState(h.sb.newState).Images().Get(job.renderTarget.image.VulkanHandle()).Aspects().Get(
			job.renderTarget.aspect).Layers().Get(
			job.renderTarget.layer).Levels().Get(
			job.renderTarget.level).Layout(), // oldLayout
		outputPreRenderLayout,                 // newLayout
		queueFamilyIgnore,                     // srcQueueFamilyIndex
		queueFamilyIgnore,                     // dstQueueFamilyIndex
		job.renderTarget.image.VulkanHandle(), // image
		NewVkImageSubresourceRange(h.sb.ta, // subresourceRange
			outputBarrierAspect,    // aspectMask
			job.renderTarget.level, // baseMipLevel
			1,                      // levelCount
			job.renderTarget.layer, // baseArrayLayer
			1,                      // layerCount
		))

	tsk.recordCmdBufCommand(func(commandBuffer VkCommandBuffer) {
		h.sb.write(h.sb.cb.VkCmdPipelineBarrier(
			commandBuffer,
			VkPipelineStageFlags(VkPipelineStageFlagBits_VK_PIPELINE_STAGE_ALL_COMMANDS_BIT),
			VkPipelineStageFlags(VkPipelineStageFlagBits_VK_PIPELINE_STAGE_ALL_COMMANDS_BIT),
			VkDependencyFlags(0),
			uint32(0),
			memory.Nullptr,
			uint32(0),
			memory.Nullptr,
			uint32(len(append(inputSrcBarriers, outputBarrier))),
			h.sb.MustAllocReadData(append(inputSrcBarriers, outputBarrier)).Ptr(),
		))
	})

	switch job.renderTarget.aspect {
	// render color or depth aspect
	case VkImageAspectFlagBits_VK_IMAGE_ASPECT_COLOR_BIT, VkImageAspectFlagBits_VK_IMAGE_ASPECT_DEPTH_BIT:
		drawInfo := ipRenderDrawInfo{
			tsk:              tsk,
			renderPass:       renderPass,
			framebuffer:      framebuffer,
			descSet:          descSet,
			pipelineLayout:   pipelineLayout,
			pipeline:         pipeline,
			aspect:           job.renderTarget.aspect,
			width:            uint32(targetLevelSize.width),
			height:           uint32(targetLevelSize.height),
			stencilWriteMask: 0,
			stencilReference: 0,
			clearStencil:     false,
		}
		h.beginRenderPassAndDraw(drawInfo)

	// render stencil aspect
	case VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT:
		// render the i'th bit of all pixels.
		for i := uint32(0); i < uint32(8); i++ {
			tsk.recordCmdBufCommand(func(commandBuffer VkCommandBuffer) {
				h.sb.write(h.sb.cb.VkCmdPipelineBarrier(
					commandBuffer,
					VkPipelineStageFlags(VkPipelineStageFlagBits_VK_PIPELINE_STAGE_ALL_COMMANDS_BIT),
					VkPipelineStageFlags(VkPipelineStageFlagBits_VK_PIPELINE_STAGE_ALL_COMMANDS_BIT),
					VkDependencyFlags(0),
					uint32(0),
					memory.Nullptr,
					uint32(0),
					memory.Nullptr,
					uint32(1),
					h.sb.MustAllocReadData([]VkImageMemoryBarrier{
						NewVkImageMemoryBarrier(h.sb.ta,
							VkStructureType_VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER, // sType
							0, // pNext
							VkAccessFlags(VkAccessFlagBits_VK_ACCESS_DEPTH_STENCIL_ATTACHMENT_WRITE_BIT), // srcAccessMask
							VkAccessFlags(VkAccessFlagBits_VK_ACCESS_DEPTH_STENCIL_ATTACHMENT_WRITE_BIT), // dstAccessMask
							VkImageLayout_VK_IMAGE_LAYOUT_DEPTH_STENCIL_ATTACHMENT_OPTIMAL,               // oldLayout
							VkImageLayout_VK_IMAGE_LAYOUT_DEPTH_STENCIL_ATTACHMENT_OPTIMAL,               // newLayout
							queueFamilyIgnore,                     // srcQueueFamilyIndex
							queueFamilyIgnore,                     // dstQueueFamilyIndex
							job.renderTarget.image.VulkanHandle(), // image
							NewVkImageSubresourceRange(h.sb.ta, // subresourceRange
								outputBarrierAspect, // aspectMask
								0,                   // baseMipLevel
								job.renderTarget.image.Info().MipLevels(), // levelCount
								0, // baseArrayLayer
								job.renderTarget.image.Info().ArrayLayers(), // layerCount
							),
						)}).Ptr(),
				))

				// Create compute pipeline
				stencilIndex := []uint32{i}
				var sib bytes.Buffer
				binary.Write(&sib, binary.LittleEndian, stencilIndex)
				h.sb.write(h.sb.cb.VkCmdPushConstants(
					commandBuffer,
					pipelineLayout.VulkanHandle(),
					VkShaderStageFlags(VkShaderStageFlagBits_VK_SHADER_STAGE_FRAGMENT_BIT),
					0,
					4,
					NewCharᶜᵖ(h.sb.MustAllocReadData(sib.Bytes()).Ptr()),
				))
			})
			drawInfo := ipRenderDrawInfo{
				tsk:              tsk,
				renderPass:       renderPass,
				framebuffer:      framebuffer,
				descSet:          descSet,
				pipelineLayout:   pipelineLayout,
				pipeline:         pipeline,
				aspect:           VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT,
				width:            uint32(targetLevelSize.width),
				height:           uint32(targetLevelSize.height),
				stencilWriteMask: 0x1 << i,
				stencilReference: 0x1 << i,
				clearStencil:     false,
			}
			if i == uint32(0) {
				drawInfo.clearStencil = true
			}
			h.beginRenderPassAndDraw(drawInfo)
		}
		dstBarriers = append(dstBarriers, NewVkImageMemoryBarrier(h.sb.ta,
			VkStructureType_VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER, // sType
			0, // pNext
			VkAccessFlags(VkAccessFlagBits_VK_ACCESS_DEPTH_STENCIL_ATTACHMENT_WRITE_BIT), // srcAccessMask
			VkAccessFlags(VkAccessFlagBits_VK_ACCESS_DEPTH_STENCIL_ATTACHMENT_WRITE_BIT), // dstAccessMask
			VkImageLayout_VK_IMAGE_LAYOUT_DEPTH_STENCIL_ATTACHMENT_OPTIMAL,               // oldLayout
			job.renderTarget.finalLayout,          // newLayout
			queueFamilyIgnore,                     // srcQueueFamilyIndex
			queueFamilyIgnore,                     // dstQueueFamilyIndex
			job.renderTarget.image.VulkanHandle(), // image
			NewVkImageSubresourceRange(h.sb.ta, // subresourceRange
				outputBarrierAspect,    // aspectMask
				job.renderTarget.level, // baseMipLevel
				1,                      // levelCount
				job.renderTarget.layer, // baseArrayLayer
				1,                      // layerCount
			),
		))
	default:
		return log.Errf(h.sb.ctx, nil, "invalid aspect: %v to render", job.renderTarget.aspect)
	}
	if len(dstBarriers) > 0 {
		tsk.recordCmdBufCommand(func(commandBuffer VkCommandBuffer) {
			h.sb.write(h.sb.cb.VkCmdPipelineBarrier(
				commandBuffer,
				VkPipelineStageFlags(VkPipelineStageFlagBits_VK_PIPELINE_STAGE_ALL_COMMANDS_BIT),
				VkPipelineStageFlags(VkPipelineStageFlagBits_VK_PIPELINE_STAGE_ALL_COMMANDS_BIT),
				VkDependencyFlags(0),
				0,
				memory.Nullptr,
				0,
				memory.Nullptr,
				uint32(len(dstBarriers)),
				h.sb.MustAllocReadData(dstBarriers).Ptr(),
			))
		})
	}

	return nil
}

// Internal functions for render handler

type ipRenderDrawInfo struct {
	tsk              *scratchTask
	renderPass       RenderPassObjectʳ
	framebuffer      FramebufferObjectʳ
	descSet          DescriptorSetObjectʳ
	pipelineLayout   PipelineLayoutObjectʳ
	pipeline         GraphicsPipelineObjectʳ
	aspect           VkImageAspectFlagBits
	width            uint32
	height           uint32
	stencilWriteMask uint32
	stencilReference uint32
	clearStencil     bool
}

func (h *ipRenderHandler) beginRenderPassAndDraw(info ipRenderDrawInfo) {
	info.tsk.recordCmdBufCommand(func(commandBuffer VkCommandBuffer) {
		h.sb.write(h.sb.cb.VkCmdBeginRenderPass(
			commandBuffer,
			h.sb.MustAllocReadData(
				NewVkRenderPassBeginInfo(h.sb.ta,
					VkStructureType_VK_STRUCTURE_TYPE_RENDER_PASS_BEGIN_INFO, // sType
					NewVoidᶜᵖ(memory.Nullptr),                                // pNext
					info.renderPass.VulkanHandle(),                           // renderPass
					info.framebuffer.VulkanHandle(),                          // framebuffer
					NewVkRect2D(h.sb.ta, // renderArea
						MakeVkOffset2D(h.sb.ta),
						NewVkExtent2D(h.sb.ta, info.width, info.height),
					),
					0, // clearValueCount
					0, // pClearValues
				)).Ptr(),
			VkSubpassContents(0),
		))

		if info.clearStencil {
			h.sb.write(h.sb.cb.VkCmdClearAttachments(
				commandBuffer,
				uint32(1),
				h.sb.MustAllocReadData([]VkClearAttachment{
					NewVkClearAttachment(h.sb.ta,
						VkImageAspectFlags(VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT), // aspectMask
						0,                         // colorAttachment
						MakeVkClearValue(h.sb.ta), // clearValue
					),
				}).Ptr(),
				uint32(1),
				h.sb.MustAllocReadData([]VkClearRect{
					NewVkClearRect(h.sb.ta,
						NewVkRect2D(h.sb.ta,
							MakeVkOffset2D(h.sb.ta),
							NewVkExtent2D(h.sb.ta, info.width, info.height),
						), // rect
						// the baseArrayLayer counts from the base layer of the
						// attachment image view.
						0, // baseArrayLayer
						1, // layerCount
					),
				}).Ptr(),
			))
		}

		h.sb.write(h.sb.cb.VkCmdBindPipeline(
			commandBuffer,
			VkPipelineBindPoint_VK_PIPELINE_BIND_POINT_GRAPHICS,
			info.pipeline.VulkanHandle(),
		))
		h.sb.write(h.sb.cb.VkCmdSetViewport(
			commandBuffer,
			uint32(0),
			uint32(1),
			NewVkViewportᶜᵖ(h.sb.MustAllocReadData(NewVkViewport(h.sb.ta,
				0, 0, // x, y
				float32(info.width), float32(info.height), // width, height
				0, 1, // minDepth, maxDepth
			)).Ptr()),
		))
		h.sb.write(h.sb.cb.VkCmdSetScissor(
			commandBuffer,
			uint32(0),
			uint32(1),
			NewVkRect2Dᶜᵖ(h.sb.MustAllocReadData(NewVkRect2D(h.sb.ta,
				MakeVkOffset2D(h.sb.ta),
				NewVkExtent2D(h.sb.ta, info.width, info.height),
			)).Ptr()),
		))
		if info.aspect == VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT {
			h.sb.write(h.sb.cb.VkCmdSetStencilWriteMask(
				commandBuffer,
				VkStencilFaceFlags(VkStencilFaceFlagBits_VK_STENCIL_FRONT_AND_BACK),
				info.stencilWriteMask,
			))
			h.sb.write(h.sb.cb.VkCmdSetStencilReference(
				commandBuffer,
				VkStencilFaceFlags(VkStencilFaceFlagBits_VK_STENCIL_FRONT_AND_BACK),
				info.stencilReference,
			))
		}
		h.sb.write(h.sb.cb.VkCmdBindDescriptorSets(
			commandBuffer,
			VkPipelineBindPoint_VK_PIPELINE_BIND_POINT_GRAPHICS,
			info.pipelineLayout.VulkanHandle(),
			0,
			1,
			h.sb.MustAllocReadData(info.descSet.VulkanHandle()).Ptr(),
			0,
			NewU32ᶜᵖ(memory.Nullptr),
		))
		h.sb.write(h.sb.cb.VkCmdDraw(
			commandBuffer,
			6, 1, 0, 0,
		))
		h.sb.write(h.sb.cb.VkCmdEndRenderPass(commandBuffer))
	})
}

func (h *ipRenderHandler) createFramebuffer(dev VkDevice, renderPass VkRenderPass, imgViews []VkImageView, width, height uint32) FramebufferObjectʳ {

	handle := VkFramebuffer(newUnusedID(true, func(x uint64) bool {
		return GetState(h.sb.newState).Framebuffers().Contains(VkFramebuffer(x))
	}))
	createInfo := NewVkFramebufferCreateInfo(h.sb.ta,
		VkStructureType_VK_STRUCTURE_TYPE_FRAMEBUFFER_CREATE_INFO, // sType
		0,                     // pNext
		0,                     // flags
		renderPass,            // renderPass
		uint32(len(imgViews)), // attachmentCount
		NewVkImageViewᶜᵖ(h.sb.MustAllocReadData(imgViews).Ptr()), // pAttachments
		width,  // width
		height, // height
		1,      // layers
	)
	h.sb.write(h.sb.cb.VkCreateFramebuffer(
		dev,
		NewVkFramebufferCreateInfoᶜᵖ(h.sb.MustAllocReadData(createInfo).Ptr()),
		memory.Nullptr,
		h.sb.MustAllocWriteData(handle).Ptr(),
		VkResult_VK_SUCCESS,
	))
	return GetState(h.sb.newState).Framebuffers().Get(handle)
}

func (h *ipRenderHandler) createImageView(dev VkDevice, img ImageObjectʳ, aspect VkImageAspectFlagBits, layer, level uint32) ImageViewObjectʳ {

	handle := VkImageView(newUnusedID(true, func(x uint64) bool {
		return GetState(h.sb.newState).ImageViews().Contains(VkImageView(x))
	}))
	h.sb.write(h.sb.cb.VkCreateImageView(
		dev,
		NewVkImageViewCreateInfoᶜᵖ(h.sb.MustAllocReadData(
			NewVkImageViewCreateInfo(h.sb.ta,
				VkStructureType_VK_STRUCTURE_TYPE_IMAGE_VIEW_CREATE_INFO, // sType
				0,                                     // pNext
				0,                                     // flags
				img.VulkanHandle(),                    // image
				VkImageViewType_VK_IMAGE_VIEW_TYPE_2D, // viewType
				img.Info().Fmt(),                      // format
				NewVkComponentMapping(h.sb.ta, // components
					VkComponentSwizzle_VK_COMPONENT_SWIZZLE_IDENTITY, // r
					VkComponentSwizzle_VK_COMPONENT_SWIZZLE_IDENTITY, // g
					VkComponentSwizzle_VK_COMPONENT_SWIZZLE_IDENTITY, // b
					VkComponentSwizzle_VK_COMPONENT_SWIZZLE_IDENTITY, // a
				),
				NewVkImageSubresourceRange(h.sb.ta, // subresourceRange
					VkImageAspectFlags(aspect), // aspectMask
					level,                      // baseMipLevel
					1,                          // levelCount
					layer,                      // baseArrayLayer
					1,                          // layerCount
				),
			)).Ptr()),
		memory.Nullptr,
		h.sb.MustAllocWriteData(handle).Ptr(),
		VkResult_VK_SUCCESS,
	))
	return GetState(h.sb.newState).ImageViews().Get(handle)
}

func (h *ipRenderHandler) allocDescriptorSet(dev VkDevice, pool VkDescriptorPool, layout VkDescriptorSetLayout) DescriptorSetObjectʳ {
	handle := VkDescriptorSet(newUnusedID(true, func(x uint64) bool {
		return GetState(h.sb.newState).DescriptorSets().Contains(VkDescriptorSet(x))
	}))
	vkAllocateDescriptorSet(h.sb, dev, pool, layout, handle)
	return GetState(h.sb.newState).DescriptorSets().Get(handle)
}

func (h *ipRenderHandler) createDescriptorPool(descSetInfo ipRenderDescriptorSetInfo) DescriptorPoolObjectʳ {

	handle := VkDescriptorPool(newUnusedID(true, func(x uint64) bool {
		return GetState(h.sb.newState).DescriptorPools().Contains(VkDescriptorPool(x))
	}))

	poolSizes := []VkDescriptorPoolSize{}
	if descSetInfo.numInputAttachments != 0 {
		poolSizes = append(poolSizes, NewVkDescriptorPoolSize(h.sb.ta,
			VkDescriptorType_VK_DESCRIPTOR_TYPE_INPUT_ATTACHMENT, // Type
			uint32(descSetInfo.numInputAttachments),              // descriptorCount
		))
	}

	vkCreateDescriptorPool(h.sb, descSetInfo.dev, VkDescriptorPoolCreateFlags(
		VkDescriptorPoolCreateFlagBits_VK_DESCRIPTOR_POOL_CREATE_FREE_DESCRIPTOR_SET_BIT),
		1, poolSizes, handle)
	return GetState(h.sb.newState).DescriptorPools().Get(handle)
}

func (h *ipRenderHandler) createRenderPass(info ipRenderPassInfo, finalLayout VkImageLayout) RenderPassObjectʳ {
	inputAttachmentRefs := make([]VkAttachmentReference, info.numInputAttachments)
	inputAttachmentDescs := make([]VkAttachmentDescription, info.numInputAttachments)
	for i := 0; i < info.numInputAttachments; i++ {
		inputAttachmentRefs[i] = NewVkAttachmentReference(h.sb.ta,
			uint32(i), // Attachment
			VkImageLayout_VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL, // Layout
		)
		inputAttachmentDescs[i] = NewVkAttachmentDescription(h.sb.ta,
			0,                                // flags
			info.inputAttachmentImageFormat,  // format
			info.inputAttachmentImageSamples, // samples
			VkAttachmentLoadOp_VK_ATTACHMENT_LOAD_OP_LOAD,          // loadOp
			VkAttachmentStoreOp_VK_ATTACHMENT_STORE_OP_DONT_CARE,   // storeOp
			VkAttachmentLoadOp_VK_ATTACHMENT_LOAD_OP_DONT_CARE,     // stencilLoadOp
			VkAttachmentStoreOp_VK_ATTACHMENT_STORE_OP_DONT_CARE,   // stencilStoreOp
			VkImageLayout_VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL, // initialLayout
			VkImageLayout_VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL, // finalLayout
		)
	}
	outputAttachmentRef := NewVkAttachmentReference(h.sb.ta,
		uint32(info.numInputAttachments), // Attachment
		// The layout will be set later according to the image aspect bits.
		VkImageLayout_VK_IMAGE_LAYOUT_UNDEFINED, // Layout
	)
	outputAttachmentDesc := NewVkAttachmentDescription(h.sb.ta,
		0,                  // flags
		info.targetFormat,  // format
		info.targetSamples, // samples
		VkAttachmentLoadOp_VK_ATTACHMENT_LOAD_OP_DONT_CARE, // loadOp
		VkAttachmentStoreOp_VK_ATTACHMENT_STORE_OP_STORE,   // storeOp
		// Keep the stencil aspect data. When rendering color or depth aspect,
		// stencil test will be disabled so stencil data won't be modified.
		VkAttachmentLoadOp_VK_ATTACHMENT_LOAD_OP_LOAD,    // stencilLoadOp
		VkAttachmentStoreOp_VK_ATTACHMENT_STORE_OP_STORE, // stencilStoreOp
		// The layout will be set later according to the image aspect bit.
		VkImageLayout_VK_IMAGE_LAYOUT_UNDEFINED, // initialLayout
		finalLayout,                             // finalLayout
	)
	subpassDesc := NewVkSubpassDescription(h.sb.ta,
		0, // flags
		VkPipelineBindPoint_VK_PIPELINE_BIND_POINT_GRAPHICS,                           // pipelineBindPoint
		uint32(info.numInputAttachments),                                              // inputAttachmentCount
		NewVkAttachmentReferenceᶜᵖ(h.sb.MustAllocReadData(inputAttachmentRefs).Ptr()), // pInputAttachments
		0, // colorAttachmentCount
		// color/depthstencil attachments will be set later according to the
		// aspect bit.
		0, // pColorAttachments
		0, // pResolveAttachments
		0, // pDepthStencilAttachment
		0, // preserveAttachmentCount
		0, // pPreserveAttachments
	)
	switch info.targetAspect {
	case VkImageAspectFlagBits_VK_IMAGE_ASPECT_COLOR_BIT:
		outputAttachmentRef.SetLayout(VkImageLayout_VK_IMAGE_LAYOUT_COLOR_ATTACHMENT_OPTIMAL)
		outputAttachmentDesc.SetInitialLayout(VkImageLayout_VK_IMAGE_LAYOUT_COLOR_ATTACHMENT_OPTIMAL)
		subpassDesc.SetColorAttachmentCount(1)
		subpassDesc.SetPColorAttachments(NewVkAttachmentReferenceᶜᵖ(h.sb.MustAllocReadData(outputAttachmentRef).Ptr()))
	case VkImageAspectFlagBits_VK_IMAGE_ASPECT_DEPTH_BIT:
		outputAttachmentRef.SetLayout(VkImageLayout_VK_IMAGE_LAYOUT_DEPTH_STENCIL_ATTACHMENT_OPTIMAL)
		outputAttachmentDesc.SetInitialLayout(VkImageLayout_VK_IMAGE_LAYOUT_DEPTH_STENCIL_ATTACHMENT_OPTIMAL)
		subpassDesc.SetPDepthStencilAttachment(NewVkAttachmentReferenceᶜᵖ(h.sb.MustAllocReadData(outputAttachmentRef).Ptr()))
	case VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT:
		outputAttachmentRef.SetLayout(VkImageLayout_VK_IMAGE_LAYOUT_DEPTH_STENCIL_ATTACHMENT_OPTIMAL)
		outputAttachmentDesc.SetInitialLayout(VkImageLayout_VK_IMAGE_LAYOUT_DEPTH_STENCIL_ATTACHMENT_OPTIMAL)
		// Rendering stencil data requires running the renderpass multiple times,
		// so do not change the image layout at the end of the renderpass
		outputAttachmentDesc.SetFinalLayout(VkImageLayout_VK_IMAGE_LAYOUT_DEPTH_STENCIL_ATTACHMENT_OPTIMAL)
		subpassDesc.SetPDepthStencilAttachment(NewVkAttachmentReferenceᶜᵖ(h.sb.MustAllocReadData(outputAttachmentRef).Ptr()))
	default:
		return NilRenderPassObjectʳ
	}

	createInfo := NewVkRenderPassCreateInfo(h.sb.ta,
		VkStructureType_VK_STRUCTURE_TYPE_RENDER_PASS_CREATE_INFO, // sType
		0,                                  // pNext
		0,                                  // flags
		uint32(info.numInputAttachments+1), // attachmentCount
		NewVkAttachmentDescriptionᶜᵖ(h.sb.MustAllocReadData( // pAttachments
			append(inputAttachmentDescs, outputAttachmentDesc),
		).Ptr()),
		1, // subpassCount
		NewVkSubpassDescriptionᶜᵖ(h.sb.MustAllocReadData(subpassDesc).Ptr()), // pSubpasses
		0, // dependencyCount
		0, // pDependencies
	)

	handle := VkRenderPass(newUnusedID(true, func(x uint64) bool {
		return GetState(h.sb.newState).RenderPasses().Contains(VkRenderPass(x))
	}))

	h.sb.write(h.sb.cb.VkCreateRenderPass(
		info.dev,
		NewVkRenderPassCreateInfoᶜᵖ(h.sb.MustAllocReadData(createInfo).Ptr()),
		memory.Nullptr,
		h.sb.MustAllocWriteData(handle).Ptr(),
		VkResult_VK_SUCCESS,
	))

	return GetState(h.sb.newState).RenderPasses().Get(handle)
}

func (h *ipRenderHandler) getOrCreateShaderModule(info ipRenderShaderInfo) (ShaderModuleObjectʳ, error) {
	if m, ok := h.shaders[info]; ok {
		return m, nil
	}
	handle := VkShaderModule(newUnusedID(true, func(x uint64) bool {
		return GetState(h.sb.newState).ShaderModules().Contains(VkShaderModule(x))
	}))
	var err error
	code := []uint32{}
	if info.isVertex {
		code, err = ipRenderVertexShaderSpirv()
	} else {
		switch info.aspect {
		case VkImageAspectFlagBits_VK_IMAGE_ASPECT_COLOR_BIT:
			code, err = ipRenderColorShaderSpirv(info.format)
		case VkImageAspectFlagBits_VK_IMAGE_ASPECT_DEPTH_BIT:
			code, err = ipRenderDepthShaderSpirv(info.format)
		case VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT:
			code, err = ipRenderStencilShaderSpirv()
		default:
			err = fmt.Errorf("Unsupported aspect bit: %v", info.aspect)
		}
	}
	if err != nil {
		return NilShaderModuleObjectʳ, log.Errf(h.sb.ctx, err, "[Generating shader SPIR-V for: %v]", info)
	}
	if len(code) == 0 {
		return NilShaderModuleObjectʳ, log.Errf(h.sb.ctx, nil, "no SPIR-V code generated")
	}
	vkCreateShaderModule(h.sb, info.dev, code, handle)
	h.shaders[info] = GetState(h.sb.newState).ShaderModules().Get(handle)
	return h.shaders[info], nil
}

func (h *ipRenderHandler) getOrCreateGraphicsPipeline(info ipGfxPipelineInfo, renderPass VkRenderPass) (GraphicsPipelineObjectʳ, error) {

	if p, ok := h.pipelines[info]; ok {
		return p, nil
	}

	vertInfo := ipRenderShaderInfo{dev: info.renderPassInfo.dev, isVertex: true}
	vertShader, err := h.getOrCreateShaderModule(vertInfo)
	if err != nil {
		return NilGraphicsPipelineObjectʳ, log.Errf(h.sb.ctx, err, "[Getting vertex shader module]")
	}
	fragShader, err := h.getOrCreateShaderModule(info.fragShaderInfo)
	if err != nil {
		return NilGraphicsPipelineObjectʳ, log.Errf(h.sb.ctx, err, "[Getting fragment shader module]")
	}

	depthTestEnable := VkBool32(0)
	depthWriteEnable := VkBool32(0)
	numColorAttachments := uint32(1)
	if info.renderPassInfo.targetAspect == VkImageAspectFlagBits_VK_IMAGE_ASPECT_DEPTH_BIT {
		depthTestEnable = VkBool32(1)
		depthWriteEnable = VkBool32(1)
		numColorAttachments = uint32(0)
	}
	stencilTestEnable := VkBool32(0)
	dynamicStates := []VkDynamicState{
		VkDynamicState_VK_DYNAMIC_STATE_VIEWPORT,
		VkDynamicState_VK_DYNAMIC_STATE_SCISSOR,
	}
	if info.renderPassInfo.targetAspect == VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT {
		stencilTestEnable = VkBool32(1)
		dynamicStates = append(dynamicStates,
			VkDynamicState_VK_DYNAMIC_STATE_STENCIL_WRITE_MASK,
			VkDynamicState_VK_DYNAMIC_STATE_STENCIL_REFERENCE,
		)
		numColorAttachments = uint32(0)
	}

	depethStencilState := NewVkPipelineDepthStencilStateCreateInfo(h.sb.ta,
		VkStructureType_VK_STRUCTURE_TYPE_PIPELINE_DEPTH_STENCIL_STATE_CREATE_INFO, // sType
		0,                                // pNext
		0,                                // flags
		depthTestEnable,                  // depthTestEnable
		depthWriteEnable,                 // depthWriteEnable
		VkCompareOp_VK_COMPARE_OP_ALWAYS, // depthCompareOp
		0,                                // depthBoundsTestEnable
		stencilTestEnable,
		NewVkStencilOpState(h.sb.ta, // front
			VkStencilOp_VK_STENCIL_OP_KEEP,    // failOp
			VkStencilOp_VK_STENCIL_OP_REPLACE, // passOp
			VkStencilOp_VK_STENCIL_OP_REPLACE, // depthFailOp
			VkCompareOp_VK_COMPARE_OP_ALWAYS,  // compareOp
			0,                                 // compareMask
			// write mask and reference must be set dynamically
			0, // writeMask
			0, // reference
		),
		NewVkStencilOpState(h.sb.ta,
			0, // failOp
			0, // passOp
			0, // depthFailOp
			0, // compareOp
			0, // compareMask
			0, // writeMask
			0, // reference
		), // back
		0.0, // minDepthBounds
		0.0, // maxDepthBounds
	)

	createInfo := NewVkGraphicsPipelineCreateInfo(h.sb.ta,
		VkStructureType_VK_STRUCTURE_TYPE_GRAPHICS_PIPELINE_CREATE_INFO, // sType
		0, // pNext
		0, // flags
		2, // stageCount
		NewVkPipelineShaderStageCreateInfoᶜᵖ(h.sb.MustAllocReadData( // pStages
			[]VkPipelineShaderStageCreateInfo{
				NewVkPipelineShaderStageCreateInfo(h.sb.ta,
					VkStructureType_VK_STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO, // sType
					0, // pNext
					0, // flags
					VkShaderStageFlagBits_VK_SHADER_STAGE_VERTEX_BIT, // stage
					vertShader.VulkanHandle(),                        // module
					NewCharᶜᵖ(h.sb.MustAllocReadData("main").Ptr()),  // pName
					NewVkSpecializationInfoᶜᵖ(memory.Nullptr),        // pSpecializationInfo
				),
				NewVkPipelineShaderStageCreateInfo(h.sb.ta,
					VkStructureType_VK_STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO, // sType
					0, // pNext
					0, // flags
					VkShaderStageFlagBits_VK_SHADER_STAGE_FRAGMENT_BIT, // stage
					fragShader.VulkanHandle(),                          // module
					NewCharᶜᵖ(h.sb.MustAllocReadData("main").Ptr()),    // pName
					NewVkSpecializationInfoᶜᵖ(memory.Nullptr),          // pSpecializationInfo
				),
			}).Ptr()),
		NewVkPipelineVertexInputStateCreateInfoᶜᵖ(h.sb.MustAllocReadData( // pVertexInputState
			NewVkPipelineVertexInputStateCreateInfo(h.sb.ta,
				VkStructureType_VK_STRUCTURE_TYPE_PIPELINE_VERTEX_INPUT_STATE_CREATE_INFO, // sType
				0, // pNext
				0, // flags
				0, // vertexBindingDescriptionCount
				NewVkVertexInputBindingDescriptionᶜᵖ(memory.Nullptr), // pVertexBindingDescriptions
				0, // vertexAttributeDescriptionCouny
				NewVkVertexInputAttributeDescriptionᶜᵖ(memory.Nullptr),
			)).Ptr()),
		NewVkPipelineInputAssemblyStateCreateInfoᶜᵖ(h.sb.MustAllocReadData( // pInputAssemblyState
			NewVkPipelineInputAssemblyStateCreateInfo(h.sb.ta,
				VkStructureType_VK_STRUCTURE_TYPE_PIPELINE_INPUT_ASSEMBLY_STATE_CREATE_INFO, // sType
				0, // pNext
				0, // flags
				VkPrimitiveTopology_VK_PRIMITIVE_TOPOLOGY_TRIANGLE_LIST, // topology
				0, // primitiveRestartEnable
			)).Ptr()),
		0, // pTessellationState
		NewVkPipelineViewportStateCreateInfoᶜᵖ(h.sb.MustAllocReadData( // pViewportState
			NewVkPipelineViewportStateCreateInfo(h.sb.ta,
				VkStructureType_VK_STRUCTURE_TYPE_PIPELINE_VIEWPORT_STATE_CREATE_INFO, // sType
				0, // pNext
				0, // flags
				1, // viewportCount
				// set viewport dynamically
				0, // pViewports
				1, // scissorCount
				// set scissor dynamically
				0, // pScissors
			)).Ptr()),
		NewVkPipelineRasterizationStateCreateInfoᶜᵖ(h.sb.MustAllocReadData( // pRasterizationState
			NewVkPipelineRasterizationStateCreateInfo(h.sb.ta,
				VkStructureType_VK_STRUCTURE_TYPE_PIPELINE_RASTERIZATION_STATE_CREATE_INFO, // sType
				0,                                  // pNext
				0,                                  // flags
				0,                                  // depthClampEnable
				0,                                  // rasterizerDiscardEnable
				VkPolygonMode_VK_POLYGON_MODE_FILL, // polygonMode
				VkCullModeFlags(VkCullModeFlagBits_VK_CULL_MODE_BACK_BIT), // cullMode
				VkFrontFace_VK_FRONT_FACE_COUNTER_CLOCKWISE,               // frontFace
				0, // depthBiasEnable
				0, // depthBiasConstantFactor
				0, // depthBiasClamp
				0, // depthBiasSlopeFactor
				1, // lineWidth
			)).Ptr()),
		NewVkPipelineMultisampleStateCreateInfoᶜᵖ(h.sb.MustAllocReadData( // pMultisampleState
			NewVkPipelineMultisampleStateCreateInfo(h.sb.ta,
				VkStructureType_VK_STRUCTURE_TYPE_PIPELINE_MULTISAMPLE_STATE_CREATE_INFO, // sType
				0, // pNext
				0, // flags
				VkSampleCountFlagBits_VK_SAMPLE_COUNT_1_BIT, // rasterizationSamples
				0, // sampleShadingEnable
				0, // minSampleShading
				0, // pSampleMask
				0, // alphaToCoverageEnable
				0, // alphaToOneEnable
			)).Ptr()),
		NewVkPipelineDepthStencilStateCreateInfoᶜᵖ(h.sb.MustAllocReadData(depethStencilState).Ptr()), // pDepthStencilState
		NewVkPipelineColorBlendStateCreateInfoᶜᵖ(h.sb.MustAllocReadData( // pColorBlendState
			NewVkPipelineColorBlendStateCreateInfo(h.sb.ta,
				VkStructureType_VK_STRUCTURE_TYPE_PIPELINE_COLOR_BLEND_STATE_CREATE_INFO, // sType
				0,                           // pNext
				0,                           // flags
				0,                           // logicOpEnable
				VkLogicOp_VK_LOGIC_OP_CLEAR, // logicOp
				numColorAttachments,         // attachmentCount
				// there is at most one color attachment
				NewVkPipelineColorBlendAttachmentStateᶜᵖ(h.sb.MustAllocReadData( // pAttachments
					NewVkPipelineColorBlendAttachmentState(h.sb.ta,
						0,                                  // blendEnable
						VkBlendFactor_VK_BLEND_FACTOR_ZERO, // srcColorBlendFactor
						VkBlendFactor_VK_BLEND_FACTOR_ONE,  // dstColorBlendFactor
						VkBlendOp_VK_BLEND_OP_ADD,          // colorBlendOp
						VkBlendFactor_VK_BLEND_FACTOR_ZERO, // srcAlphaBlendFactor
						VkBlendFactor_VK_BLEND_FACTOR_ONE,  // dstAlphaBlendFactor
						VkBlendOp_VK_BLEND_OP_ADD,          // alphaBlendOp
						0xf,                                // colorWriteMask
					)).Ptr()),
				NilF32ː4ᵃ, // blendConstants
			)).Ptr()),
		NewVkPipelineDynamicStateCreateInfoᶜᵖ(h.sb.MustAllocReadData( // pDynamicState
			NewVkPipelineDynamicStateCreateInfo(h.sb.ta,
				VkStructureType_VK_STRUCTURE_TYPE_PIPELINE_DYNAMIC_STATE_CREATE_INFO, // sType
				0,                          // pNext
				0,                          // flags
				uint32(len(dynamicStates)), // dynamicStateCount
				NewVkDynamicStateᶜᵖ(h.sb.MustAllocReadData(dynamicStates).Ptr()), // pDynamicStates
			)).Ptr()),
		info.pipelineLayout, // layout
		renderPass,          // renderPass
		0,                   // subpass
		0,                   // basePipelineHandle
		0,                   // basePipelineIndex
	)

	handle := VkPipeline(newUnusedID(true, func(x uint64) bool {
		return GetState(h.sb.newState).GraphicsPipelines().Contains(VkPipeline(x))
	}))

	h.sb.write(h.sb.cb.VkCreateGraphicsPipelines(
		info.renderPassInfo.dev, VkPipelineCache(0), uint32(1),
		NewVkGraphicsPipelineCreateInfoᶜᵖ(h.sb.MustAllocReadData(createInfo).Ptr()),
		memory.Nullptr, h.sb.MustAllocWriteData(handle).Ptr(), VkResult_VK_SUCCESS,
	))

	h.pipelines[info] = GetState(h.sb.newState).GraphicsPipelines().Get(handle)
	return h.pipelines[info], nil
}

func (h *ipRenderHandler) getOrCreatePipelineLayout(descSetInfo ipRenderDescriptorSetInfo) PipelineLayoutObjectʳ {
	if l, ok := h.pipelineLayouts[descSetInfo]; ok {
		return l
	}
	handle := VkPipelineLayout(newUnusedID(true, func(x uint64) bool {
		return GetState(h.sb.newState).PipelineLayouts().Contains(VkPipelineLayout(x))
	}))
	descriptorSet := h.getOrCreateDescriptorSetLayout(descSetInfo)
	pushConstantRanges := []VkPushConstantRange{}
	if descSetInfo.pushConstant {
		pushConstantRanges = append(pushConstantRanges, NewVkPushConstantRange(h.sb.ta,
			VkShaderStageFlags(VkShaderStageFlagBits_VK_SHADER_STAGE_FRAGMENT_BIT), // stageFlags
			0, // offset
			4, // size
		))
	}
	vkCreatePipelineLayout(h.sb, descSetInfo.dev,
		[]VkDescriptorSetLayout{descriptorSet.VulkanHandle()},
		pushConstantRanges,
		handle,
	)
	h.pipelineLayouts[descSetInfo] = GetState(h.sb.newState).PipelineLayouts().Get(handle)
	return h.pipelineLayouts[descSetInfo]
}

func (h *ipRenderHandler) getOrCreateDescriptorSetLayout(descSetInfo ipRenderDescriptorSetInfo) DescriptorSetLayoutObjectʳ {

	if l, ok := h.descriptorSetLayouts[descSetInfo]; ok {
		return l
	}

	handle := VkDescriptorSetLayout(newUnusedID(true, func(x uint64) bool {
		return GetState(h.sb.newState).DescriptorSetLayouts().Contains(VkDescriptorSetLayout(x))
	}))

	bindings := []VkDescriptorSetLayoutBinding{}
	if descSetInfo.numInputAttachments != 0 {
		bindings = append(bindings, NewVkDescriptorSetLayoutBinding(h.sb.ta,
			ipRenderInputAttachmentBinding,                                         // binding
			VkDescriptorType_VK_DESCRIPTOR_TYPE_INPUT_ATTACHMENT,                   // descriptorType
			uint32(descSetInfo.numInputAttachments),                                // descriptorCount
			VkShaderStageFlags(VkShaderStageFlagBits_VK_SHADER_STAGE_FRAGMENT_BIT), // stageFlags
			0, // pImmutableSamplers
		))
	}

	vkCreateDescriptorSetLayout(h.sb, descSetInfo.dev, bindings, handle)
	h.descriptorSetLayouts[descSetInfo] = GetState(h.sb.newState).DescriptorSetLayouts().Get(handle)
	return h.descriptorSetLayouts[descSetInfo]
}

// Buffer->Image copy session

// ipBufImgCopyJob describes how the data in the src image to be copied to dst
// images, i.e. which aspect of the src image should be copied to which aspect
// of which dst image, and the final layout of the dst images. Note that the
// source of the data is the state block of the source image (data owner), not
// the VkImage handle, so such a copy does not modify the state of the src image
type ipBufImgCopyJob struct {
	srcAspectsToDsts map[VkImageAspectFlagBits]*ipBufImgCopyDst
	srcImg           ImageObjectʳ
}

// ipBufImgCopyDst contains a list of dst images whose dst aspect will be written
// by a serial of image copy operations.
type ipBufImgCopyDst struct {
	dstImgs   []ImageObjectʳ
	dstAspect VkImageAspectFlagBits
}

func newImagePrimerBufferImageCopyJob(srcImg ImageObjectʳ) *ipBufImgCopyJob {
	return &ipBufImgCopyJob{
		srcAspectsToDsts: map[VkImageAspectFlagBits]*ipBufImgCopyDst{},
		srcImg:           srcImg,
	}
}

func (s *ipBufImgCopyJob) addDst(ctx context.Context, srcAspect, dstAspect VkImageAspectFlagBits, dstImgs ...ImageObjectʳ) error {
	if s.srcAspectsToDsts[srcAspect] == nil {
		s.srcAspectsToDsts[srcAspect] = &ipBufImgCopyDst{
			dstImgs:   []ImageObjectʳ{},
			dstAspect: dstAspect,
		}
	}
	if s.srcAspectsToDsts[srcAspect].dstAspect != dstAspect {
		return log.Errf(ctx, nil, "new dstAspect:%v does not match with the existing one: %v", dstAspect, s.srcAspectsToDsts[srcAspect].dstAspect)
	}
	s.srcAspectsToDsts[srcAspect].dstImgs = append(s.srcAspectsToDsts[srcAspect].dstImgs, dstImgs...)
	return nil
}

type ipBufferImageCopySession struct {
	// Copies for each dst image, in the same order of content, all copies have offsets start at 0.
	copies map[ImageObjectʳ][]VkBufferImageCopy
	// The buffer content of each VkBufferImageCopy for each dst image, all sub-range fill info
	// starts their range at 0.
	content map[ImageObjectʳ][]bufferSubRangeFillInfo
	// The index of each dst images, in case the source data image format is
	// wider than staging image format, so that multple destination images are
	// used.
	indices   map[ImageObjectʳ]int
	totalSize uint64
	// The source and destination image for this copy session.
	job *ipBufImgCopyJob
	sb  *stateBuilder
}

// interfaces to interact with image primer

func newImagePrimerBufferImageCopySession(sb *stateBuilder, job *ipBufImgCopyJob) *ipBufferImageCopySession {
	h := &ipBufferImageCopySession{
		copies:  map[ImageObjectʳ][]VkBufferImageCopy{},
		content: map[ImageObjectʳ][]bufferSubRangeFillInfo{},
		indices: map[ImageObjectʳ]int{},
		job:     job,
		sb:      sb,
	}
	for _, dst := range job.srcAspectsToDsts {
		for _, img := range dst.dstImgs {
			h.copies[img] = []VkBufferImageCopy{}
			h.content[img] = []bufferSubRangeFillInfo{}
		}
	}
	return h
}

func (h *ipBufferImageCopySession) collectCopiesFromSubresourceRange(srcRng VkImageSubresourceRange) {
	walkImageSubresourceRange(h.sb, h.job.srcImg, srcRng,
		func(aspect VkImageAspectFlagBits, layer, level uint32, levelSize byteSizeAndExtent) {
			extent := NewVkExtent3D(h.sb.ta,
				uint32(levelSize.width),
				uint32(levelSize.height),
				uint32(levelSize.depth),
			)
			for dstIndex, dstImg := range h.job.srcAspectsToDsts[aspect].dstImgs {
				// dstIndex is reserved for handling wide channel image format
				// like R64G64B64A64
				// TODO: handle wide format
				bufFillInfo, bufImgCopy, err := h.getCopyAndData(
					dstImg, h.job.srcAspectsToDsts[aspect].dstAspect,
					h.job.srcImg, aspect, layer, level, MakeVkOffset3D(h.sb.ta),
					extent)
				if err != nil {
					log.E(h.sb.ctx, "[Getting VkBufferImageCopy and raw data for priming data at image: %v, aspect: %v, layer: %v, level: %v] %v", h.job.srcImg.VulkanHandle(), aspect, layer, level, err)
					continue
				}
				h.copies[dstImg] = append(h.copies[dstImg], bufImgCopy)
				h.content[dstImg] = append(h.content[dstImg], bufFillInfo)
				h.indices[dstImg] = dstIndex
				h.totalSize += bufFillInfo.size()
			}
		})
}

func (h *ipBufferImageCopySession) collectCopiesFromSparseImageBindings() {
	walkSparseImageMemoryBindings(h.sb, h.job.srcImg,
		func(aspect VkImageAspectFlagBits, layer, level uint32, blockData SparseBoundImageBlockInfoʳ) {
			for dstIndex, dstImg := range h.job.srcAspectsToDsts[aspect].dstImgs {
				// dstIndex is reserved for handling wide channel image format
				// TODO: handle wide format
				_ = dstIndex
				bufFillInfo, bufImgCopy, err := h.getCopyAndData(
					dstImg, h.job.srcAspectsToDsts[aspect].dstAspect,
					h.job.srcImg, aspect, layer, level, blockData.Offset(),
					blockData.Extent())
				if err != nil {
					log.E(h.sb.ctx, "[Getting VkBufferImageCopy and raw data from sparse image binding at image: %v, aspect: %v, layer: %v, level: %v, offset: %v, extent: %v] %v", h.job.srcImg.VulkanHandle(), aspect, layer, level, blockData.Offset(), blockData.Extent(), err)
					continue
				}
				h.copies[dstImg] = append(h.copies[dstImg], bufImgCopy)
				h.content[dstImg] = append(h.content[dstImg], bufFillInfo)
				h.indices[dstImg] = dstIndex
				h.totalSize += bufFillInfo.size()
			}
		})
}

func (h *ipBufferImageCopySession) rolloutBufCopies(queue VkQueue, initLayouts, finalLayouts ipLayoutInfo) error {

	if h.totalSize == 0 || len(h.copies) == 0 || len(h.content) == 0 {
		return log.Errf(h.sb.ctx, nil, "no content for buf->img copy")
	}

	if len(h.copies) != len(h.content) {
		return log.Errf(h.sb.ctx, nil, "mismatch number of VkBufferImageCopy: %v and buffer content pieces: %v", len(h.copies), len(h.content))
	}

	for _, dst := range h.job.srcAspectsToDsts {
		for _, dstImg := range dst.dstImgs {
			preCopyDstImgBarriers := []VkImageMemoryBarrier{}
			for layer := uint32(0); layer < dstImg.Info().ArrayLayers(); layer++ {
				for level := uint32(0); level < dstImg.Info().MipLevels(); level++ {
					barrier := NewVkImageMemoryBarrier(h.sb.ta,
						VkStructureType_VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER, // sType
						0, // pNext
						VkAccessFlags((VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT-1)|VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT), // srcAccessMask
						VkAccessFlags((VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT-1)|VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT), // dstAccessMask
						initLayouts.layoutOf(dst.dstAspect, layer, level),                                                          // oldLayout
						VkImageLayout_VK_IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL,                                                         // newLayout
						queueFamilyIgnore,     // srcQueueFamilyIndex
						queueFamilyIgnore,     // dstQueueFamilyIndex
						dstImg.VulkanHandle(), // image
						NewVkImageSubresourceRange(h.sb.ta, // subresourceRange
							ipImageBarrierAspectFlags(dst.dstAspect, dstImg.Info().Fmt()), // aspectMask
							level, // baseMipLevel
							1,     // levelCount
							layer, // baseArrayLayer
							1,     // layerCount
						),
					)
					preCopyDstImgBarriers = append(preCopyDstImgBarriers, barrier)
				}
			}

			postCopyDstImgBarriers := []VkImageMemoryBarrier{}
			for layer := uint32(0); layer < dstImg.Info().ArrayLayers(); layer++ {
				for level := uint32(0); level < dstImg.Info().MipLevels(); level++ {
					barrier := NewVkImageMemoryBarrier(h.sb.ta,
						VkStructureType_VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER, // sType
						0, // pNext
						VkAccessFlags((VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT-1)|VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT), // srcAccessMask
						VkAccessFlags((VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT-1)|VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT), // dstAccessMask
						VkImageLayout_VK_IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL,                                                         // oldLayout
						finalLayouts.layoutOf(dst.dstAspect, layer, level),                                                         // newLayout
						queueFamilyIgnore,     // srcQueueFamilyIndex
						queueFamilyIgnore,     // dstQueueFamilyIndex
						dstImg.VulkanHandle(), // image
						NewVkImageSubresourceRange(h.sb.ta, // subresourceRange
							ipImageBarrierAspectFlags(dst.dstAspect, dstImg.Info().Fmt()), // aspectMask
							level, // baseMipLevel
							1,     // levelCount
							layer, // baseArrayLayer
							1,     // layerCount
						),
					)
					postCopyDstImgBarriers = append(postCopyDstImgBarriers, barrier)
				}
			}

			preCopyDstLayoutTransitionTsk := h.sb.newScratchTaskOnQueue(queue)
			preCopyDstLayoutTransitionTsk.recordCmdBufCommand(func(commandBuffer VkCommandBuffer) {
				h.sb.write(h.sb.cb.VkCmdPipelineBarrier(
					commandBuffer,
					VkPipelineStageFlags(VkPipelineStageFlagBits_VK_PIPELINE_STAGE_ALL_COMMANDS_BIT),
					VkPipelineStageFlags(VkPipelineStageFlagBits_VK_PIPELINE_STAGE_ALL_COMMANDS_BIT),
					VkDependencyFlags(0),
					uint32(0),
					memory.Nullptr,
					uint32(0),
					memory.Nullptr,
					uint32(len(preCopyDstImgBarriers)),
					h.sb.MustAllocReadData(preCopyDstImgBarriers).Ptr(),
				))
			})
			if err := preCopyDstLayoutTransitionTsk.commit(); err != nil {
				return log.Errf(h.sb.ctx, err, "[Committing pre-copy destination image layout transition commands]")
			}

			notProcessedCopies := h.copies[dstImg]
			notProcessedContent := h.content[dstImg]
			for len(notProcessedCopies) != 0 && len(notProcessedContent) != 0 {
				copies := []VkBufferImageCopy{}
				bufContent := []bufferSubRangeFillInfo{}
				bufOffset := uint64(0)
				tsk := h.sb.newScratchTaskOnQueue(queue)
				addIthCopyAndContent := func(i int) {
					copy := notProcessedCopies[i]
					copy.SetBufferOffset(VkDeviceSize(bufOffset))
					copies = append(copies, copy)
					content := notProcessedContent[i]
					content.setOffsetInBuffer(bufOffset)
					bufContent = append(bufContent, content)
					bufOffset += content.size()
				}

				addIthCopyAndContent(0)
				for i := 1; i < len(notProcessedCopies); i++ {
					if nextMultipleOf(bufOffset+notProcessedContent[i].size(), 256) > scratchBufferSize {
						break
					}
					addIthCopyAndContent(i)
				}

				notProcessedCopies = notProcessedCopies[len(copies):]
				notProcessedContent = notProcessedContent[len(copies):]
				// scratch buffer will be destroyed once the scratch task finishes.
				scratchBuffer := tsk.newBuffer(bufContent, VkBufferUsageFlagBits_VK_BUFFER_USAGE_TRANSFER_SRC_BIT)

				tsk.recordCmdBufCommand(func(commandBuffer VkCommandBuffer) {
					h.sb.write(h.sb.cb.VkCmdPipelineBarrier(
						commandBuffer,
						VkPipelineStageFlags(VkPipelineStageFlagBits_VK_PIPELINE_STAGE_ALL_COMMANDS_BIT),
						VkPipelineStageFlags(VkPipelineStageFlagBits_VK_PIPELINE_STAGE_ALL_COMMANDS_BIT),
						VkDependencyFlags(0),
						uint32(0),
						memory.Nullptr,
						uint32(1),
						h.sb.MustAllocReadData(
							NewVkBufferMemoryBarrier(h.sb.ta,
								VkStructureType_VK_STRUCTURE_TYPE_BUFFER_MEMORY_BARRIER, // sType
								0, // pNext
								VkAccessFlags((VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT-1)|VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT), // srcAccessMask
								VkAccessFlags((VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT-1)|VkAccessFlagBits_VK_ACCESS_MEMORY_WRITE_BIT), // dstAccessMask
								queueFamilyIgnore,       // srcQueueFamilyIndex
								queueFamilyIgnore,       // dstQueueFamilyIndex
								scratchBuffer,           // buffer
								0,                       // offset
								VkDeviceSize(bufOffset), // size
							)).Ptr(),
						uint32(0),
						memory.Nullptr,
					))
				})

				tsk.recordCmdBufCommand(func(commandBuffer VkCommandBuffer) {
					h.sb.write(h.sb.cb.VkCmdCopyBufferToImage(
						commandBuffer,
						scratchBuffer,
						dstImg.VulkanHandle(),
						VkImageLayout_VK_IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL,
						uint32(len(copies)),
						h.sb.MustAllocReadData(copies).Ptr(),
					))
				})

				tsk.recordCmdBufCommand(func(commandBuffer VkCommandBuffer) {
					h.sb.write(h.sb.cb.VkCmdPipelineBarrier(
						commandBuffer,
						VkPipelineStageFlags(VkPipelineStageFlagBits_VK_PIPELINE_STAGE_ALL_COMMANDS_BIT),
						VkPipelineStageFlags(VkPipelineStageFlagBits_VK_PIPELINE_STAGE_ALL_COMMANDS_BIT),
						VkDependencyFlags(0),
						uint32(0),
						memory.Nullptr,
						uint32(0),
						memory.Nullptr,
						uint32(0),
						memory.Nullptr,
					))
				})
				if err := tsk.commit(); err != nil {
					return log.Errf(h.sb.ctx, err, "[Committing scratch buffer filling and image copy commands, scratch buffer size: %v]", bufOffset)
				}
			}
			postCopyDstLayoutTransitionTsk := h.sb.newScratchTaskOnQueue(queue)
			postCopyDstLayoutTransitionTsk.recordCmdBufCommand(func(commandBuffer VkCommandBuffer) {
				h.sb.write(h.sb.cb.VkCmdPipelineBarrier(
					commandBuffer,
					VkPipelineStageFlags(VkPipelineStageFlagBits_VK_PIPELINE_STAGE_ALL_COMMANDS_BIT),
					VkPipelineStageFlags(VkPipelineStageFlagBits_VK_PIPELINE_STAGE_ALL_COMMANDS_BIT),
					VkDependencyFlags(0),
					uint32(0),
					memory.Nullptr,
					uint32(0),
					memory.Nullptr,
					uint32(len(postCopyDstImgBarriers)),
					h.sb.MustAllocReadData(postCopyDstImgBarriers).Ptr(),
				))
			})
			if err := postCopyDstLayoutTransitionTsk.commit(); err != nil {
				return log.Errf(h.sb.ctx, err, "[Committing post-copy destination image layout transition commands]")
			}
		}
	}
	return nil
}

// internal functions of ipBufferCopSessionr

// getCopyAndData returns the buffer content and the VkBufferImageCopy struct
// to be used to conduct the data copy from the specific subresource of the src
// image to the corresponding subresource of the dst image. The returned content
// and the VkBufferImageCopy assume the copy will be carried out with a buffer
// range starts from 0, i.e. the bufferOffset of VkBufferImageCopy is 0, and the
// bufferSubRangeFillInfo's range begin at 0.
func (h *ipBufferImageCopySession) getCopyAndData(dstImg ImageObjectʳ, dstAspect VkImageAspectFlagBits, srcImg ImageObjectʳ, srcAspect VkImageAspectFlagBits, layer, level uint32, opaqueBlockOffset VkOffset3D, opaqueBlockExtent VkExtent3D) (bufferSubRangeFillInfo, VkBufferImageCopy, error) {
	var err error
	bufImgCopy := NewVkBufferImageCopy(h.sb.ta,
		VkDeviceSize(0), // bufferOffset
		0,               // bufferRowLength
		0,               // bufferImageHeight
		NewVkImageSubresourceLayers(h.sb.ta, // imageSubresource
			VkImageAspectFlags(dstAspect), // aspectMask
			level,                         // mipLevel
			layer,                         // baseArrayLayer
			1,                             // layerCount
		),
		opaqueBlockOffset, // imageOffset
		opaqueBlockExtent, // imageExtent
	)
	srcImgDataOffset := uint64(h.sb.levelSize(NewVkExtent3D(h.sb.ta,
		uint32(opaqueBlockOffset.X()),
		uint32(opaqueBlockOffset.Y()),
		uint32(opaqueBlockOffset.Z()),
	), srcImg.Info().Fmt(), 0, srcAspect).levelSize)
	srcImgDataSizeInBytes := uint64(h.sb.levelSize(
		opaqueBlockExtent,
		srcImg.Info().Fmt(),
		0, srcAspect).levelSize)
	dataSlice := srcImg.
		Aspects().Get(srcAspect).
		Layers().Get(layer).
		Levels().Get(level).
		Data().Slice(srcImgDataOffset, srcImgDataOffset+srcImgDataSizeInBytes)

	errorIfUnexpectedLength := func(dataLen uint64) error {
		dstLevelSize := h.sb.levelSize(opaqueBlockExtent, dstImg.Info().Fmt(), 0, dstAspect)
		if dataLen != dstLevelSize.alignedLevelSizeInBuf {
			return log.Errf(h.sb.ctx, nil, "size of unpackedData data does not match expectation, actual: %v, expected: %v, srcFmt: %v, dstFmt: %v", dataLen, dstLevelSize.alignedLevelSizeInBuf, srcImg.Info().Fmt(), dstImg.Info().Fmt())
		}
		return nil
	}

	unpackedData := []uint8{}

	if dstImg.Info().Fmt() != srcImg.Info().Fmt() {
		// dstImg format is different with the srcImage format, the dst image
		// should be a staging image.
		srcVkFmt := srcImg.Info().Fmt()
		data := dataSlice.MustRead(h.sb.ctx, nil, h.sb.oldState, nil)
		if srcVkFmt == VkFormat_VK_FORMAT_E5B9G9R9_UFLOAT_PACK32 {
			data, srcVkFmt, err = ebgrDataToRGB32SFloat(data, opaqueBlockExtent)
			if err != nil {
				return bufferSubRangeFillInfo{}, bufImgCopy, log.Errf(h.sb.ctx, err, "[Converting data in VK_FORMAT_E5B9G9R9_UFLOAT_PACK32 to VK_FORMAT_R32G32B32_SFLOAT]")
			}
		}
		unpackedData, _, err = unpackDataForPriming(h.sb.ctx, data, srcVkFmt, srcAspect)
		if err != nil {
			return bufferSubRangeFillInfo{}, bufImgCopy, log.Errf(h.sb.ctx, err, "[Unpacking data from format: %v aspect: %v]", srcVkFmt, srcAspect)
		}

	} else if srcAspect == VkImageAspectFlagBits_VK_IMAGE_ASPECT_DEPTH_BIT {
		// srcImg format is the same to the dstImage format, the data is ready to
		// be used directly, except when the src image is a dpeth 24 UNORM one.
		if (srcImg.Info().Fmt() == VkFormat_VK_FORMAT_D24_UNORM_S8_UINT) ||
			(srcImg.Info().Fmt() == VkFormat_VK_FORMAT_X8_D24_UNORM_PACK32) {
			data := dataSlice.MustRead(h.sb.ctx, nil, h.sb.oldState, nil)
			unpackedData, _, err = unpackDataForPriming(h.sb.ctx, data, srcImg.Info().Fmt(), srcAspect)
			if err != nil {
				return bufferSubRangeFillInfo{}, bufImgCopy, log.Errf(h.sb.ctx, err, "[Unpacking data from format: %v aspect: %v]", srcImg.Info().Fmt(), srcAspect)
			}
		}
	}

	if len(unpackedData) != 0 {
		extendToMultipleOf8(&unpackedData)
		if err := errorIfUnexpectedLength(uint64(len(unpackedData))); err != nil {
			return bufferSubRangeFillInfo{}, bufImgCopy, err
		}
	} else if dataSlice.Size()%8 != 0 {
		unpackedData = dataSlice.MustRead(h.sb.ctx, nil, h.sb.oldState, nil)
		extendToMultipleOf8(&unpackedData)
		if err := errorIfUnexpectedLength(uint64(len(unpackedData))); err != nil {
			return bufferSubRangeFillInfo{}, bufImgCopy, err
		}
	} else {
		if err := errorIfUnexpectedLength(dataSlice.Size()); err != nil {
			return bufferSubRangeFillInfo{}, bufImgCopy, err
		}
	}

	if len(unpackedData) != 0 {
		return newBufferSubRangeFillInfoFromNewData(unpackedData, 0), bufImgCopy, nil
	}
	return newBufferSubRangeFillInfoFromSlice(h.sb, dataSlice, 0), bufImgCopy, nil
}

// free functions

func extendToMultipleOf8(dataPtr *[]uint8) {
	l := uint64(len(*dataPtr))
	nl := nextMultipleOf(l, 8)
	zeros := make([]uint8, nl-l)
	*dataPtr = append(*dataPtr, zeros...)
}

func unpackDataForPriming(ctx context.Context, data []uint8, srcFmt VkFormat, aspect VkImageAspectFlagBits) ([]uint8, VkFormat, error) {
	ctx = log.Enter(ctx, "unpackDataForPriming")
	var sf *image.Format
	var err error
	var dstFmt VkFormat
	switch aspect {
	case VkImageAspectFlagBits_VK_IMAGE_ASPECT_COLOR_BIT:
		sf, err = getImageFormatFromVulkanFormat(srcFmt)
		if err != nil {
			return []uint8{}, dstFmt, log.Errf(ctx, err, "[Getting image.Format for VkFormat: %v, aspect: %v]", srcFmt, aspect)
		}
		dstFmt = stagingColorImageBufferFormat

	case VkImageAspectFlagBits_VK_IMAGE_ASPECT_DEPTH_BIT:
		sf, err = getDepthImageFormatFromVulkanFormat(srcFmt)
		if err != nil {
			return []uint8{}, dstFmt, log.Errf(ctx, err, "[Getting image.Format for VkFormat: %v, aspect: %v]", srcFmt, aspect)
		}
		dstFmt = stagingDepthStencilImageBufferFormat

	case VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT:
		sf, err = getImageFormatFromVulkanFormat(VkFormat_VK_FORMAT_S8_UINT)
		if err != nil {
			return []uint8{}, dstFmt, log.Errf(ctx, err, "[Getting image.Format for VkFormat: %v, aspect: %v]", srcFmt, aspect)
		}
		dstFmt = stagingDepthStencilImageBufferFormat

	default:
		return []uint8{}, dstFmt, log.Errf(ctx, nil, "unsupported aspect: %v", aspect)
	}

	df, err := getImageFormatFromVulkanFormat(dstFmt)
	if err != nil {
		return []uint8{}, dstFmt, log.Errf(ctx, err, "[Getting image.Format for VkFormat %v]", dstFmt)
	}
	unpacked, err := unpackData(ctx, data, sf, df)
	if err != nil {
		return []uint8{}, dstFmt, err
	}
	return unpacked, dstFmt, nil
}

func unpackData(ctx context.Context, data []uint8, srcFmt, dstFmt *image.Format) ([]uint8, error) {
	ctx = log.Enter(ctx, "unpackData")
	var err error
	if srcFmt.GetUncompressed() == nil {
		return []uint8{}, log.Errf(ctx, nil, "compressed format: %v is not supported", srcFmt)
	}
	if dstFmt.GetUncompressed() == nil {
		return []uint8{}, log.Errf(ctx, nil, "compressed format: %v is not supported", dstFmt)
	}
	sf := proto.Clone(srcFmt).(*image.Format).GetUncompressed().GetFormat()
	df := proto.Clone(dstFmt).(*image.Format).GetUncompressed().GetFormat()

	// The casting rule is described as below:
	// If the data layout is UNORM, unsigned extends the src data to uint32
	// If the data layout is SNORM, signed extends the src data to sint32
	// If the data layout is UINT, unsigned extends the src data to uint32
	// If the data layout is SINT, signed extends the src data to sint32
	// If the data layout is FLOAT, cast the src data to sfloat32
	// Note that, the staging image formats are always UINT32, the data within
	// the staging image should be encoded as float32, if the source data is
	// in float point type. The data will be bitcasted to float32 in the shader
	// when rendering to the state block image in the replay side.
	// If the source data is in normalized type, it will be treated as integer,
	// and will be normalized in the shader when rendering in the replay side.
	// Also, to keep data in SRGB untouched, the sampling curve of the source
	// format will be changed to linear.

	// Modify the src and dst format stream to follow the rule above.
	for _, sc := range sf.Components {
		if sc.Channel == stream.Channel_Depth || sc.Channel == stream.Channel_Stencil {
			sc.Channel = stream.Channel_Red
		}
		dc, _ := df.Component(sc.Channel)
		if dc == nil {
			return []uint8{}, log.Errf(ctx, nil, "[Building src format: %v] unsuppored channel in source data format: %v", sf, sc.Channel)
		}
		sc.Sampling = stream.Linear
		if sc.GetDataType().GetInteger() != nil {
			dc.DataType = &stream.U32
			dc.Sampling = stream.Linear
			if sc.GetDataType().GetSigned() {
				dc.DataType = &stream.S32
			}
		} else if sc.GetDataType().GetFloat() != nil {
			dc.DataType = &stream.F32
			dc.Sampling = stream.Linear
		} else {
			return []uint8{}, log.Errf(ctx, nil, "[Building dst format for: %v] %s", sf, "DataType other than stream.Integer and stream.Float are not handled.")
		}
	}

	converted, err := stream.Convert(df, sf, data)
	if err != nil {
		return []uint8{}, log.Errf(ctx, err, "[Converting data from %v to %v]", sf, df)
	}
	return converted, nil
}

func ebgrDataToRGB32SFloat(data []uint8, extent VkExtent3D) ([]uint8, VkFormat, error) {
	dstFmt := VkFormat_VK_FORMAT_R32G32B32_SFLOAT
	sf, err := getImageFormatFromVulkanFormat(VkFormat_VK_FORMAT_E5B9G9R9_UFLOAT_PACK32)
	if err != nil {
		return []uint8{}, dstFmt, err
	}
	df, err := getImageFormatFromVulkanFormat(dstFmt)
	if err != nil {
		return []uint8{}, dstFmt, err
	}
	retData, err := image.Convert(data, int(extent.Width()), int(extent.Height()), int(extent.Depth()), sf, df)
	if err != nil {
		return []uint8{}, dstFmt, err
	}
	return retData, dstFmt, nil
}

func isDenseBound(img ImageObjectʳ) bool {
	return img.PlaneMemoryInfo().Len() > 0 && func() bool {
		for _, m := range img.PlaneMemoryInfo().All() {
			if m.BoundMemory().IsNil() {
				return false
			}
		}
		return true
	}()
}

func isSparseBound(img ImageObjectʳ) bool {
	return (img.SparseImageMemoryBindings().Len() > 0 || img.OpaqueSparseMemoryBindings().Len() > 0) && ((uint64(img.Info().Flags()) & uint64(VkImageCreateFlagBits_VK_IMAGE_CREATE_SPARSE_BINDING_BIT)) != 0)
}

func isSparseResidency(img ImageObjectʳ) bool {
	return isSparseBound(img) &&
		((uint32(img.Info().Flags()) & uint32(VkImageCreateFlagBits_VK_IMAGE_CREATE_SPARSE_RESIDENCY_BIT)) != 0)
}

func vkCreateImage(sb *stateBuilder, dev VkDevice, info ImageInfo, handle VkImage) {
	pNext := NewVoidᶜᵖ(memory.Nullptr)
	if !info.DedicatedAllocationNV().IsNil() {
		pNext = NewVoidᶜᵖ(sb.MustAllocReadData(
			NewVkDedicatedAllocationImageCreateInfoNV(sb.ta,
				VkStructureType_VK_STRUCTURE_TYPE_DEDICATED_ALLOCATION_IMAGE_CREATE_INFO_NV, // sType
				0, // pNext
				info.DedicatedAllocationNV().DedicatedAllocation(), // dedicatedAllocation
			),
		).Ptr())
	}

	create := sb.cb.VkCreateImage(
		dev, sb.MustAllocReadData(
			NewVkImageCreateInfo(sb.ta,
				VkStructureType_VK_STRUCTURE_TYPE_IMAGE_CREATE_INFO, // sType
				pNext,                                   // pNext
				info.Flags(),                            // flags
				info.ImageType(),                        // imageType
				info.Fmt(),                              // format
				info.Extent(),                           // extent
				info.MipLevels(),                        // mipLevels
				info.ArrayLayers(),                      // arrayLayers
				info.Samples(),                          // samples
				info.Tiling(),                           // tiling
				info.Usage(),                            // usage
				info.SharingMode(),                      // sharingMode
				uint32(info.QueueFamilyIndices().Len()), // queueFamilyIndexCount
				NewU32ᶜᵖ(sb.MustUnpackReadMap(info.QueueFamilyIndices().All()).Ptr()), // pQueueFamilyIndices
				info.InitialLayout(), // initialLayout
			)).Ptr(),
		memory.Nullptr,
		sb.MustAllocWriteData(handle).Ptr(),
		VkResult_VK_SUCCESS,
	)

	if sb.s.Images().Contains(handle) {
		obj := sb.s.Images().Get(handle)
		fetchedReq := MakeFetchedImageMemoryRequirements(sb.newState.Arena)
		for p, pmi := range obj.PlaneMemoryInfo().All() {
			fetchedReq.PlaneBitsToMemoryRequirements().Add(p, pmi.MemoryRequirements())
		}
		for b, sparseReq := range obj.SparseMemoryRequirements().All() {
			fetchedReq.AspectBitsToSparseMemoryRequirements().Add(b, sparseReq)
		}
		create.Extras().Add(fetchedReq)
	}

	sb.write(create)
}

func vkGetImageMemoryRequirements(sb *stateBuilder, dev VkDevice, handle VkImage, memReq VkMemoryRequirements) {
	sb.write(sb.cb.VkGetImageMemoryRequirements(
		dev, handle, sb.MustAllocWriteData(memReq).Ptr(),
	))
}

func vkAllocateMemory(sb *stateBuilder, dev VkDevice, size VkDeviceSize, memTypeIndex uint32, handle VkDeviceMemory) {
	sb.write(sb.cb.VkAllocateMemory(
		dev,
		NewVkMemoryAllocateInfoᶜᵖ(sb.MustAllocReadData(
			NewVkMemoryAllocateInfo(sb.ta,
				VkStructureType_VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO, // sType
				0,            // pNext
				size,         // allocationSize
				memTypeIndex, // memoryTypeIndex
			)).Ptr()),
		memory.Nullptr,
		sb.MustAllocWriteData(handle).Ptr(),
		VkResult_VK_SUCCESS,
	))
}

func vkBindImageMemory(sb *stateBuilder, dev VkDevice, img VkImage, mem VkDeviceMemory, offset VkDeviceSize) {
	sb.write(sb.cb.VkBindImageMemory(
		dev, img, mem, offset, VkResult_VK_SUCCESS,
	))
}

func vkCreateDescriptorSetLayout(sb *stateBuilder, dev VkDevice, bindings []VkDescriptorSetLayoutBinding, handle VkDescriptorSetLayout) {
	sb.write(sb.cb.VkCreateDescriptorSetLayout(
		dev,
		sb.MustAllocReadData(NewVkDescriptorSetLayoutCreateInfo(sb.ta,
			VkStructureType_VK_STRUCTURE_TYPE_DESCRIPTOR_SET_LAYOUT_CREATE_INFO, // sType
			0,                     // pNext
			0,                     // flags
			uint32(len(bindings)), // bindingCount
			NewVkDescriptorSetLayoutBindingᶜᵖ(sb.MustAllocReadData(bindings).Ptr()), // pBindings
		)).Ptr(),
		NewVoidᶜᵖ(memory.Nullptr),
		sb.MustAllocWriteData(handle).Ptr(),
		VkResult_VK_SUCCESS,
	))
}

func vkAllocateDescriptorSet(sb *stateBuilder, dev VkDevice, pool VkDescriptorPool, layout VkDescriptorSetLayout, handle VkDescriptorSet) {
	sb.write(sb.cb.VkAllocateDescriptorSets(
		dev,
		sb.MustAllocReadData(NewVkDescriptorSetAllocateInfo(sb.ta,
			VkStructureType_VK_STRUCTURE_TYPE_DESCRIPTOR_SET_ALLOCATE_INFO, // sType
			0,    // pNext
			pool, // descriptorPool
			1,    // descriptorSetCount
			NewVkDescriptorSetLayoutᶜᵖ(sb.MustAllocReadData(layout).Ptr()), // pSetLayouts
		)).Ptr(),
		sb.MustAllocWriteData(handle).Ptr(),
		VkResult_VK_SUCCESS,
	))
}

func vkCreatePipelineLayout(sb *stateBuilder, dev VkDevice, setLayouts []VkDescriptorSetLayout, pushConstantRanges []VkPushConstantRange, handle VkPipelineLayout) {
	createInfo := NewVkPipelineLayoutCreateInfo(sb.ta,
		VkStructureType_VK_STRUCTURE_TYPE_PIPELINE_LAYOUT_CREATE_INFO, // sType
		0,                       // pNext
		0,                       // flags
		uint32(len(setLayouts)), // setLayoutCount
		NewVkDescriptorSetLayoutᶜᵖ(sb.MustAllocReadData(setLayouts).Ptr()),       // pSetLayouts
		uint32(len(pushConstantRanges)),                                          // pushConstantRangeCount
		NewVkPushConstantRangeᶜᵖ(sb.MustAllocReadData(pushConstantRanges).Ptr()), // pPushConstantRanges
	)
	sb.write(sb.cb.VkCreatePipelineLayout(
		dev,
		NewVkPipelineLayoutCreateInfoᶜᵖ(sb.MustAllocReadData(createInfo).Ptr()),
		memory.Nullptr,
		sb.MustAllocWriteData(handle).Ptr(),
		VkResult_VK_SUCCESS,
	))
}

func vkCreateShaderModule(sb *stateBuilder, dev VkDevice, code []uint32, handle VkShaderModule) {
	createInfo := NewVkShaderModuleCreateInfo(sb.ta,
		VkStructureType_VK_STRUCTURE_TYPE_SHADER_MODULE_CREATE_INFO, // sType
		0,                        // pNext
		0,                        // flags
		memory.Size(len(code)*4), // codeSize
		NewU32ᶜᵖ(sb.MustAllocReadData(code).Ptr()), // pCode
	)

	descriptors, err := shadertools.ParseAllDescriptorSets(code)
	u := MakeDescriptorInfo(sb.ta)
	dsc := u.Descriptors()
	if err != nil {
		log.E(sb.ctx, "Could not parse SPIR-V")
	} else {
		for name, desc := range descriptors {
			d := NewU32ːDescriptorUsageᵐ(sb.ta)
			for _, set := range desc {
				for _, binding := range set {
					d.Add(uint32(d.Len()),
						NewDescriptorUsage(
							sb.ta,
							binding.Set,
							binding.Binding,
							binding.DescriptorCount))
				}
			}
			dsc.Add(name, d)
		}
	}
	csb := sb.cb.VkCreateShaderModule(
		dev,
		NewVkShaderModuleCreateInfoᶜᵖ(sb.MustAllocReadData(createInfo).Ptr()),
		memory.Nullptr,
		sb.MustAllocWriteData(handle).Ptr(),
		VkResult_VK_SUCCESS,
	)
	csb.Extras().Add(u)
	sb.write(csb)
}

func vkCreateDescriptorPool(sb *stateBuilder, dev VkDevice, flags VkDescriptorPoolCreateFlags, maxSet uint32, poolSizes []VkDescriptorPoolSize, handle VkDescriptorPool) {
	sb.write(sb.cb.VkCreateDescriptorPool(
		dev,
		sb.MustAllocReadData(NewVkDescriptorPoolCreateInfo(sb.ta,
			VkStructureType_VK_STRUCTURE_TYPE_DESCRIPTOR_POOL_CREATE_INFO, // sType
			0,                      // pNext
			flags,                  // flags
			maxSet,                 // maxSets
			uint32(len(poolSizes)), // poolSizeCount
			NewVkDescriptorPoolSizeᶜᵖ(sb.MustAllocReadData(poolSizes).Ptr()), // pPoolSizes
		)).Ptr(),
		memory.Nullptr,
		sb.MustAllocWriteData(handle).Ptr(),
		VkResult_VK_SUCCESS,
	))
}

func writeDescriptorSet(sb *stateBuilder, dev VkDevice, descSet VkDescriptorSet, dstBinding, dstArrayElement uint32, descType VkDescriptorType, imgInfoList []VkDescriptorImageInfo, bufInfoList []VkDescriptorBufferInfo, texelBufInfoList []VkBufferView) {
	write := NewVkWriteDescriptorSet(sb.ta,
		VkStructureType_VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET, // sType
		0,               // pNext
		descSet,         // dstSet
		dstBinding,      // dstBinding
		dstArrayElement, // dstArrayElement
		uint32(len(imgInfoList)+len(bufInfoList)+len(texelBufInfoList)), // descriptorCount
		descType, // descriptorType
		NewVkDescriptorImageInfoᶜᵖ(sb.MustAllocReadData(imgInfoList).Ptr()),  // pImageInfo
		NewVkDescriptorBufferInfoᶜᵖ(sb.MustAllocReadData(bufInfoList).Ptr()), // pBufferInfo
		NewVkBufferViewᶜᵖ(sb.MustAllocReadData(texelBufInfoList).Ptr()),      // pTexelBufferView
	)

	sb.write(sb.cb.VkUpdateDescriptorSets(
		dev,
		1,
		NewVkWriteDescriptorSetᶜᵖ(sb.MustAllocReadData(write).Ptr()),
		0,
		memory.Nullptr,
	))
}

func walkImageSubresourceRange(sb *stateBuilder, img ImageObjectʳ, rng VkImageSubresourceRange, f func(aspect VkImageAspectFlagBits, layer, level uint32, levelSize byteSizeAndExtent)) {
	layerCount, _ := subImageSubresourceLayerCount(sb.ctx, nil, api.CmdNoID, nil, sb.oldState, nil, 0, nil, nil, img, rng)
	levelCount, _ := subImageSubresourceLevelCount(sb.ctx, nil, api.CmdNoID, nil, sb.oldState, nil, 0, nil, nil, img, rng)
	for _, aspect := range sb.imageAspectFlagBits(img, rng.AspectMask()) {
		for i := uint32(0); i < levelCount; i++ {
			level := rng.BaseMipLevel() + i
			levelSize := sb.levelSize(img.Info().Extent(), img.Info().Fmt(), level, aspect)
			for j := uint32(0); j < layerCount; j++ {
				layer := rng.BaseArrayLayer() + j
				f(aspect, layer, level, levelSize)
			}
		}
	}
}

func walkSparseImageMemoryBindings(sb *stateBuilder, img ImageObjectʳ, f func(aspect VkImageAspectFlagBits, layer, level uint32, blockData SparseBoundImageBlockInfoʳ)) {
	for aspect, aspectData := range img.SparseImageMemoryBindings().All() {
		for layer, layerData := range aspectData.Layers().All() {
			for level, levelData := range layerData.Levels().All() {
				for _, blockData := range levelData.Blocks().All() {
					f(VkImageAspectFlagBits(aspect), layer, level, blockData)
				}
			}
		}
	}
}

func roundUp(dividend, divisor uint64) uint64 {
	return (dividend + divisor - 1) / divisor
}
